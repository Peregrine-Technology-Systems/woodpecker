// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRSAKeyPEM returns a freshly generated 2048-bit RSA private key encoded as
// PKCS#1 PEM, plus the parsed key for verifying signatures in-test.
func testRSAKeyPEM(t *testing.T) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return pemBytes, key
}

// appAPI is an httptest stand-in for GitHub's App API: the per-repo
// installation lookup (GET /repos/{owner}/{repo}/installation) and the
// installation-token mint (POST /app/installations/{id}/access_tokens). Each
// endpoint counts calls and can be overridden per test.
type appAPI struct {
	*httptest.Server
	installations map[string]int64 // owner -> installation id (for the default resolve)
	resolves      atomic.Int64
	mints         sync.Map // installation id -> *atomic.Int64

	// overrides; nil => default happy handler.
	resolveFn func(w http.ResponseWriter, owner string)
	mintFn    func(w http.ResponseWriter, installationID, mintNo int64)
}

func newAppAPI(t *testing.T, installations map[string]int64) *appAPI {
	t.Helper()
	api := &appAPI{installations: installations}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, len(r.Header.Get("Authorization")) > len("Bearer "), "App JWT bearer must be present")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
			api.resolves.Add(1)
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // repos/{owner}/{repo}/installation
			require.Len(t, parts, 4)
			owner := parts[1]
			if api.resolveFn != nil {
				api.resolveFn(w, owner)
				return
			}
			id, ok := installations[owner]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"message":"not installed on %s"}`, owner)
				return
			}
			fmt.Fprintf(w, `{"id":%d}`, id)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // app/installations/{id}/access_tokens
			require.Len(t, parts, 4)
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			cnt, _ := api.mints.LoadOrStore(id, &atomic.Int64{})
			n := cnt.(*atomic.Int64).Add(1)
			if api.mintFn != nil {
				api.mintFn(w, id, n)
				return
			}
			writeToken(w, fmt.Sprintf("ghs_%d_%d", id, n), time.Now().Add(time.Hour))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(api.Close)
	return api
}

func (api *appAPI) apiBase() string { return api.Server.URL + "/" }

func (api *appAPI) mintCount(id int64) int64 {
	if v, ok := api.mints.Load(id); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func writeToken(w http.ResponseWriter, token string, expiresAt time.Time) {
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, expiresAt.UTC().Format(time.RFC3339))
}

func newTestAppAuth(t *testing.T, pemKey []byte, api string) *appAuth {
	t.Helper()
	a, err := newAppAuthFromOpts(Opts{AppID: 123, AppPrivateKey: pemKey}, api, http.DefaultClient)
	require.NoError(t, err)
	require.NotNil(t, a)
	return a
}

func TestNewAppAuthFromOpts(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)

	t.Run("unconfigured returns nil,nil", func(t *testing.T) {
		a, err := newAppAuthFromOpts(Opts{}, "https://api.github.com/", http.DefaultClient)
		require.NoError(t, err)
		assert.Nil(t, a)
	})

	t.Run("partial config fails loudly", func(t *testing.T) {
		_, err := newAppAuthFromOpts(Opts{AppID: 1}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partially configured")

		_, err = newAppAuthFromOpts(Opts{AppPrivateKey: pemKey}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partially configured")
	})

	t.Run("unparseable key fails loudly", func(t *testing.T) {
		_, err := newAppAuthFromOpts(Opts{AppID: 1, AppPrivateKey: []byte("not a pem key")}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private key")
	})

	t.Run("full valid config succeeds", func(t *testing.T) {
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppPrivateKey: pemKey}, "https://api.github.com/", http.DefaultClient)
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.Equal(t, int64(123), a.appID)
	})
}

func TestAppAuthSignAppJWT(t *testing.T) {
	pemKey, key := testRSAKeyPEM(t)
	a := newTestAppAuth(t, pemKey, "https://api.github.com/")

	frozen := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return frozen }

	signed, err := a.signAppJWT()
	require.NoError(t, err)

	parsed, err := jwt.Parse(signed, func(_ *jwt.Token) (any, error) { return &key.PublicKey, nil },
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithTimeFunc(func() time.Time { return frozen }))
	require.NoError(t, err)
	require.True(t, parsed.Valid)

	claims, ok := parsed.Claims.(jwt.MapClaims)
	require.True(t, ok)
	assert.Equal(t, "123", claims["iss"])
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	assert.Equal(t, frozen.Add(-appJWTBackdate).Unix(), iat)
	assert.Equal(t, frozen.Add(appJWTLifetime).Unix(), exp)
	assert.LessOrEqual(t, exp-frozen.Unix(), int64(600), "exp must be <= 10m from now (GitHub ceiling)")
}

func TestAppAuthInstallationToken_MintAndCache(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	api := newAppAPI(t, map[string]int64{"acme": 100})
	a := newTestAppAuth(t, pemKey, api.apiBase())

	tok, err := a.installationToken(context.Background(), "acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, "ghs_100_1", tok)

	// Second call for the same owner must hit both caches — no re-resolve,
	// no re-mint.
	tok2, err := a.installationToken(context.Background(), "acme", "gadgets")
	require.NoError(t, err)
	assert.Equal(t, "ghs_100_1", tok2)
	assert.Equal(t, int64(1), api.resolves.Load(), "installation must be resolved once per owner")
	assert.Equal(t, int64(1), api.mintCount(100), "cached token must not re-mint")
}

func TestAppAuthInstallationToken_PerAccountInstallations(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	// Two owners => two distinct installations, each with its own token bucket.
	api := newAppAPI(t, map[string]int64{"peregrine": 100, "amalc": 200})
	a := newTestAppAuth(t, pemKey, api.apiBase())

	tokOrg, err := a.installationToken(context.Background(), "peregrine", "woodpecker")
	require.NoError(t, err)
	tokPersonal, err := a.installationToken(context.Background(), "amalc", "uscgaux-website-checker")
	require.NoError(t, err)

	assert.Equal(t, "ghs_100_1", tokOrg)
	assert.Equal(t, "ghs_200_1", tokPersonal, "personal-account repo must use its own installation, not the org's")
	assert.Equal(t, int64(1), api.mintCount(100))
	assert.Equal(t, int64(1), api.mintCount(200))
	assert.Equal(t, int64(2), api.resolves.Load())

	// Owner casing must not defeat the cache.
	_, err = a.installationToken(context.Background(), "Peregrine", "other")
	require.NoError(t, err)
	assert.Equal(t, int64(2), api.resolves.Load(), "owner resolution is case-insensitive")
}

func TestAppAuthInstallationToken_RefreshBeforeExpiry(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	api := newAppAPI(t, map[string]int64{"acme": 100})
	api.mintFn = func(w http.ResponseWriter, id, n int64) {
		writeToken(w, fmt.Sprintf("ghs_%d_%d", id, n), base.Add(time.Hour))
	}
	a := newTestAppAuth(t, pemKey, api.apiBase())

	a.now = func() time.Time { return base }
	tok, err := a.installationToken(context.Background(), "acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, "ghs_100_1", tok)

	// Inside the refresh margin: re-mint rather than hand back a token about
	// to expire mid-request.
	a.now = func() time.Time { return base.Add(time.Hour).Add(-appTokenRefreshMargin).Add(time.Second) }
	tok, err = a.installationToken(context.Background(), "acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, "ghs_100_2", tok)
	assert.Equal(t, int64(2), api.mintCount(100))

	// Just before the margin: still-valid token is reused.
	a.now = func() time.Time { return base.Add(time.Hour).Add(-appTokenRefreshMargin).Add(-time.Second) }
	_, err = a.installationToken(context.Background(), "acme", "widgets")
	require.NoError(t, err)
	assert.Equal(t, int64(2), api.mintCount(100), "token outside refresh margin must be reused")
}

func TestAppAuthInstallationToken_ResolveErrorsFailLoud(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)

	t.Run("app not installed on account (404)", func(t *testing.T) {
		api := newAppAPI(t, map[string]int64{"acme": 100}) // "other" not present => 404
		a := newTestAppAuth(t, pemKey, api.apiBase())
		_, err := a.installationToken(context.Background(), "other", "repo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "installation lookup")
		assert.Contains(t, err.Error(), "404")
	})

	t.Run("installation lookup returns zero id", func(t *testing.T) {
		api := newAppAPI(t, nil)
		api.resolveFn = func(w http.ResponseWriter, _ string) { fmt.Fprint(w, `{"id":0}`) }
		a := newTestAppAuth(t, pemKey, api.apiBase())
		_, err := a.installationToken(context.Background(), "acme", "repo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no installation id")
	})

	t.Run("installation lookup malformed json", func(t *testing.T) {
		api := newAppAPI(t, nil)
		api.resolveFn = func(w http.ResponseWriter, _ string) { fmt.Fprint(w, `{bad`) }
		a := newTestAppAuth(t, pemKey, api.apiBase())
		_, err := a.installationToken(context.Background(), "acme", "repo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode")
	})
}

func TestAppAuthInstallationToken_MintErrorsFailLoud(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)

	cases := []struct {
		name   string
		mintFn func(w http.ResponseWriter, id, n int64)
		want   string
	}{
		{"non-201 status", func(w http.ResponseWriter, _, _ int64) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"rate limited"}`)
		}, "403"},
		{"empty token", func(w http.ResponseWriter, _, _ int64) {
			writeToken(w, "", time.Now().Add(time.Hour))
		}, "no token"},
		{"missing expiry", func(w http.ResponseWriter, _, _ int64) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"ghs_x"}`)
		}, "expiry"},
		{"malformed json", func(w http.ResponseWriter, _, _ int64) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{not json`)
		}, "decode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAppAPI(t, map[string]int64{"acme": 100})
			api.mintFn = tc.mintFn
			a := newTestAppAuth(t, pemKey, api.apiBase())
			_, err := a.installationToken(context.Background(), "acme", "repo")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, fmt.Errorf("boom: body read failed") }
func (errReadCloser) Close() error             { return nil }

func TestAppAuthInstallationToken_TransportErrorsFailLoud(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)

	t.Run("http client error", func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial tcp: connection refused")
		})}
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppPrivateKey: pemKey}, "https://api.github.com/", hc)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background(), "acme", "repo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "request failed")
	})

	t.Run("malformed api base fails request build", func(t *testing.T) {
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppPrivateKey: pemKey}, "http://bad\x7fhost/", http.DefaultClient)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background(), "acme", "repo")
		require.Error(t, err)
	})

	t.Run("body read error", func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: errReadCloser{}, Header: make(http.Header)}, nil
		})}
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppPrivateKey: pemKey}, "https://api.github.com/", hc)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background(), "acme", "repo")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read")
	})
}

func TestAppAuthInstallationToken_ConcurrentMintsOnce(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	api := newAppAPI(t, map[string]int64{"acme": 100})
	a := newTestAppAuth(t, pemKey, api.apiBase())

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := a.installationToken(context.Background(), "acme", "widgets"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), api.resolves.Load(), "concurrent callers must share a single resolve")
	assert.Equal(t, int64(1), api.mintCount(100), "concurrent callers must share a single mint")
}

func TestNewServerClient(t *testing.T) {
	c := &client{API: defaultAPI, url: defaultURL}
	ctx := context.Background()

	t.Run("unconfigured falls back to user token", func(t *testing.T) {
		c.appAuth = nil
		gh, err := c.newServerClient(ctx, "acme", "repo", "user-token")
		require.NoError(t, err)
		assert.NotNil(t, gh)
	})

	t.Run("configured-but-broken fails loud, no user-token fallback", func(t *testing.T) {
		pemKey, _ := testRSAKeyPEM(t)
		api := newAppAPI(t, nil) // every resolve 404s
		c.appAuth = newTestAppAuth(t, pemKey, api.apiBase())
		gh, err := c.newServerClient(ctx, "acme", "repo", "user-token")
		require.Error(t, err)
		assert.Nil(t, gh, "must not return a user-token client when App auth is configured but failing")
		assert.Contains(t, err.Error(), "configured but failed")
	})

	t.Run("configured and working authenticates as app", func(t *testing.T) {
		pemKey, _ := testRSAKeyPEM(t)
		api := newAppAPI(t, map[string]int64{"acme": 100})
		c.appAuth = newTestAppAuth(t, pemKey, api.apiBase())
		gh, err := c.newServerClient(ctx, "acme", "repo", "user-token")
		require.NoError(t, err)
		assert.NotNil(t, gh)
	})
}
