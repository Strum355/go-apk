// Copyright 2023 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apk

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"

	sign "github.com/chainguard-dev/go-apk/pkg/signature"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-retryablehttp"
	"go.lsp.dev/uri"
	"go.opentelemetry.io/otel"
)

var signatureFileRegex = regexp.MustCompile(`^\.SIGN\.RSA\.(.*\.rsa\.pub)$`)

// This is terrible but simpler than plumbing around a cache for now.
// We just hold the parsed index in memory rather than re-parsing it every time,
// which requires gunzipping, which is (somewhat) expensive.
var globalIndexCache = &indexCache{
	modtimes: map[string]time.Time{},
}

type indexResult struct {
	idx *APKIndex
	err error
}

type indexCache struct {
	// For remote indexes.
	onces sync.Map

	// For local indexes.
	sync.Mutex
	modtimes map[string]time.Time

	// repoBase -> indexResult
	indexes sync.Map
}

func (i *indexCache) get(ctx context.Context, u string, keys map[string][]byte, arch string, opts *indexOpts) (*APKIndex, error) {
	if strings.HasPrefix(u, "https://") {
		// We don't want remote indexes to change while we're running.
		once, _ := i.onces.LoadOrStore(u, &sync.Once{})
		once.(*sync.Once).Do(func() {
			idx, err := getRepositoryIndex(ctx, u, keys, arch, opts)
			i.indexes.Store(u, indexResult{
				idx: idx,
				err: err,
			})
		})
	} else {
		i.Lock()
		defer i.Unlock()

		// We do expect local indexes to change, so we check modtimes.
		stat, err := os.Stat(u)
		if err != nil {
			return nil, nil
		}

		mod := stat.ModTime()
		before, ok := i.modtimes[u]
		if !ok || mod.After(before) {
			// If this is the first time or it has changed since the last time...
			idx, err := getRepositoryIndex(ctx, u, keys, arch, opts)
			i.indexes.Store(u, indexResult{
				idx: idx,
				err: err,
			})
			i.modtimes[u] = mod
		}
	}

	v, ok := i.indexes.Load(u)
	if !ok {
		panic(fmt.Errorf("did not see index %q after writing it", u))
	}
	result := v.(indexResult)

	return result.idx, result.err
}

// IndexURL full URL to the index file for the given repo and arch
func IndexURL(repo, arch string) string {
	return fmt.Sprintf("%s/%s/%s", repo, arch, indexFilename)
}

// GetRepositoryIndexes returns the indexes for the named repositories, keys and archs.
// The signatures for each index are verified unless ignoreSignatures is set to true.
// The key-value pairs in the map for `keys` are the name of the key and the contents of the key.
// The name is just indicative. If it finds a match, it will use it. Else, it will try all keys.
func GetRepositoryIndexes(ctx context.Context, repos []string, keys map[string][]byte, arch string, options ...IndexOption) (indexes []NamedIndex, err error) {
	ctx, span := otel.Tracer("go-apk").Start(ctx, "GetRepositoryIndexes")
	defer span.End()

	opts := &indexOpts{}
	for _, opt := range options {
		opt(opts)
	}

	for _, repo := range repos {
		// does it start with a pin?
		var (
			repoName string
			repoURL  = repo
		)
		if strings.HasPrefix(repo, "@") {
			// it's a pinned repository, get the name
			parts := strings.Fields(repo)
			if len(parts) < 2 {
				return nil, fmt.Errorf("invalid repository line: %q", repo)
			}
			repoName = parts[0][1:]
			repoURL = parts[1]
		}

		u := IndexURL(repoURL, arch)
		repoBase := fmt.Sprintf("%s/%s", repoURL, arch)

		index, err := globalIndexCache.get(ctx, u, keys, arch, opts)
		if err != nil {
			return nil, err
		}

		// Can happen for fs.ErrNotExist in file scheme, we just ignore it.
		if index == nil {
			continue
		}

		repoRef := Repository{URI: repoBase}
		indexes = append(indexes, NewNamedRepositoryWithIndex(repoName, repoRef.WithIndex(index)))
	}
	return indexes, nil
}

func getRepositoryIndex(ctx context.Context, u string, keys map[string][]byte, arch string, opts *indexOpts) (*APKIndex, error) {
	// Normalize the repo as a URI, so that local paths
	// are translated into file:// URLs, allowing them to be parsed
	// into a url.URL{}.
	var (
		b     []byte
		asURL *url.URL
		err   error
	)
	if strings.HasPrefix(u, "https://") {
		asURL, err = url.Parse(u)
	} else {
		// Attempt to parse non-https elements into URI's so they are translated into
		// file:// URLs allowing them to parse into a url.URL{}
		asURL, err = url.Parse(string(uri.New(u)))
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse repo as URI: %w", err)
	}

	switch asURL.Scheme {
	case "file":
		b, err = os.ReadFile(u)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("failed to read repository %s: %w", u, err)
			}
			return nil, nil
		}
	case "https":
		client := opts.httpClient
		if client == nil {
			rhttp := retryablehttp.NewClient()
			rhttp.Logger = hclog.Default()
			client = rhttp.StandardClient()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, asURL.String(), nil)
		if err != nil {
			return nil, err
		}
		// if the repo URL contains HTTP Basic Auth credentials, add them to the request
		if asURL.User != nil {
			user := asURL.User.Username()
			pass, _ := asURL.User.Password()
			req.SetBasicAuth(user, pass)
		}

		// This will return a body that retries requests using Range requests if Read() hits an error.
		rrt := newRangeRetryTransport(ctx, client)
		res, err := rrt.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("unable to get repository index at %s: %w", u, err)
		}
		switch res.StatusCode {
		case http.StatusOK:
			// this is fine
		case http.StatusNotFound:
			return nil, fmt.Errorf("repository index not found for architecture %s at %s", arch, u)
		default:
			return nil, fmt.Errorf("unexpected status code %d when getting repository index for architecture %s at %s", res.StatusCode, arch, u)
		}
		defer res.Body.Close()
		buf := bytes.NewBuffer(nil)
		if _, err := io.Copy(buf, res.Body); err != nil {
			return nil, fmt.Errorf("unable to read repository index at %s: %w", u, err)
		}
		b = buf.Bytes()
	default:
		return nil, fmt.Errorf("repository scheme %s not supported", asURL.Scheme)
	}

	// validate the signature
	if !opts.ignoreSignatures {
		buf := bytes.NewReader(b)
		gzipReader, err := gzip.NewReader(buf)
		if err != nil {
			return nil, fmt.Errorf("unable to create gzip reader for repository index: %w", err)
		}
		// set multistream to false, so we can read each part separately;
		// the first part is the signature, the second is the index, which should be
		// verified.
		gzipReader.Multistream(false)
		defer gzipReader.Close()

		tarReader := tar.NewReader(gzipReader)

		// read the signature
		signatureFile, err := tarReader.Next()
		if err != nil {
			return nil, fmt.Errorf("failed to read signature from repository index: %w", err)
		}
		matches := signatureFileRegex.FindStringSubmatch(signatureFile.Name)
		if len(matches) != 2 {
			return nil, fmt.Errorf("failed to find key name in signature file name: %s", signatureFile.Name)
		}
		signature, err := io.ReadAll(tarReader)
		if err != nil {
			return nil, fmt.Errorf("failed to read signature from repository index: %w", err)
		}
		// with multistream false, we should read the next one
		if _, err := tarReader.Next(); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("unexpected error reading from tgz: %w", err)
		}
		// we now have the signature bytes and name, get the contents of the rest;
		// this should be everything else in the raw gzip file as is.
		allBytes := len(b)
		unreadBytes := buf.Len()
		readBytes := allBytes - unreadBytes
		indexData := b[readBytes:]

		indexDigest, err := sign.HashData(indexData)
		if err != nil {
			return nil, err
		}
		// now we can check the signature
		if keys == nil {
			return nil, fmt.Errorf("no keys provided to verify signature")
		}
		var verified bool
		keyData, ok := keys[matches[1]]
		if ok {
			if err := sign.RSAVerifySHA1Digest(indexDigest, signature, keyData); err != nil {
				verified = false
			}
		}
		if !verified {
			for _, keyData := range keys {
				if err := sign.RSAVerifySHA1Digest(indexDigest, signature, keyData); err == nil {
					verified = true
					break
				}
			}
		}
		if !verified {
			return nil, fmt.Errorf("no key found to verify signature for keyfile %s; tried all other keys as well", matches[1])
		}
	}
	// with a valid signature, convert it to an ApkIndex
	index, err := IndexFromArchive(io.NopCloser(bytes.NewReader(b)))
	if err != nil {
		return nil, fmt.Errorf("unable to read convert repository index bytes to index struct at %s: %w", u, err)
	}

	return index, err
}

type indexOpts struct {
	ignoreSignatures bool
	httpClient       *http.Client
}
type IndexOption func(*indexOpts)

func WithIgnoreSignatures(ignoreSignatures bool) IndexOption {
	return func(o *indexOpts) {
		o.ignoreSignatures = ignoreSignatures
	}
}

func WithHTTPClient(c *http.Client) IndexOption {
	return func(o *indexOpts) {
		o.httpClient = c
	}
}
