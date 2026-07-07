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

// tokenServer is an httptest server standing in for GitHub's installation-token
// endpoint. It counts mints and lets each test shape the response.
type tokenServer struct {
	*httptest.Server
	mints   atomic.Int64
	handler func(w http.ResponseWriter, mintNo int64)
}

func newTokenServer(t *testing.T, handler func(w http.ResponseWriter, mintNo int64)) *tokenServer {
	t.Helper()
	ts := &tokenServer{handler: handler}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Contains(t, r.URL.Path, "/app/installations/42/access_tokens")
		assert.True(t, len(r.Header.Get("Authorization")) > len("Bearer "), "App JWT bearer must be present")
		n := ts.mints.Add(1)
		ts.handler(w, n)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// apiBase returns the server URL with the trailing slash appAuth expects.
func (ts *tokenServer) apiBase() string { return ts.Server.URL + "/" }

func okTokenHandler(token string, expiresAt time.Time) func(http.ResponseWriter, int64) {
	return func(w http.ResponseWriter, _ int64) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, expiresAt.UTC().Format(time.RFC3339))
	}
}

func newTestAppAuth(t *testing.T, pemKey []byte, api string) *appAuth {
	t.Helper()
	a, err := newAppAuthFromOpts(Opts{
		AppID:             123,
		AppInstallationID: 42,
		AppPrivateKey:     pemKey,
	}, api, http.DefaultClient)
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
		// id + installation but no key
		_, err := newAppAuthFromOpts(Opts{AppID: 1, AppInstallationID: 2}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partially configured")

		// key but no ids
		_, err = newAppAuthFromOpts(Opts{AppPrivateKey: pemKey}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partially configured")
	})

	t.Run("unparseable key fails loudly", func(t *testing.T) {
		_, err := newAppAuthFromOpts(Opts{
			AppID: 1, AppInstallationID: 2, AppPrivateKey: []byte("not a pem key"),
		}, "x", http.DefaultClient)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private key")
	})

	t.Run("full valid config succeeds", func(t *testing.T) {
		a, err := newAppAuthFromOpts(Opts{
			AppID: 123, AppInstallationID: 42, AppPrivateKey: pemKey,
		}, "https://api.github.com/", http.DefaultClient)
		require.NoError(t, err)
		require.NotNil(t, a)
		assert.Equal(t, int64(123), a.appID)
		assert.Equal(t, int64(42), a.installationID)
	})
}

func TestAppAuthSignAppJWT(t *testing.T) {
	pemKey, key := testRSAKeyPEM(t)
	a := newTestAppAuth(t, pemKey, "https://api.github.com/")

	// Freeze the clock so we can assert iat backdating and exp bounds exactly.
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
	// iat is backdated for clock skew; exp is within GitHub's 10-minute ceiling.
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	assert.Equal(t, frozen.Add(-appJWTBackdate).Unix(), iat)
	assert.Equal(t, frozen.Add(appJWTLifetime).Unix(), exp)
	assert.LessOrEqual(t, exp-frozen.Unix(), int64(600), "exp must be <= 10m from now (GitHub ceiling)")
}

func TestAppAuthInstallationToken_MintAndCache(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	ts := newTokenServer(t, okTokenHandler("ghs_minted", time.Now().Add(time.Hour)))
	a := newTestAppAuth(t, pemKey, ts.apiBase())

	tok, err := a.installationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_minted", tok)

	// Second call within validity must hit the cache, not re-mint.
	tok2, err := a.installationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_minted", tok2)
	assert.Equal(t, int64(1), ts.mints.Load(), "cached token must not re-mint")
}

func TestAppAuthInstallationToken_RefreshBeforeExpiry(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	// Each mint returns a distinct token and an expiry 1h out from a base time.
	base := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	ts := newTokenServer(t, func(w http.ResponseWriter, n int64) {
		okTokenHandler(fmt.Sprintf("ghs_%d", n), base.Add(time.Hour))(w, n)
	})
	a := newTestAppAuth(t, pemKey, ts.apiBase())

	// Clock at base: first mint.
	a.now = func() time.Time { return base }
	tok, err := a.installationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_1", tok)

	// Advance to inside the refresh margin (expiry - margin + 1s): must re-mint
	// rather than hand back a token about to expire mid-request.
	a.now = func() time.Time { return base.Add(time.Hour).Add(-appTokenRefreshMargin).Add(time.Second) }
	tok, err = a.installationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ghs_2", tok)
	assert.Equal(t, int64(2), ts.mints.Load())

	// Just before the margin, the still-valid token is reused.
	a.now = func() time.Time { return base.Add(time.Hour).Add(-appTokenRefreshMargin).Add(-time.Second) }
	_, err = a.installationToken(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), ts.mints.Load(), "token outside refresh margin must be reused")
}

func TestAppAuthInstallationToken_MintErrorFailsLoud(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)

	t.Run("non-201 status", func(t *testing.T) {
		ts := newTokenServer(t, func(w http.ResponseWriter, _ int64) {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"rate limited"}`)
		})
		a := newTestAppAuth(t, pemKey, ts.apiBase())
		_, err := a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "403")
	})

	t.Run("empty token in response", func(t *testing.T) {
		ts := newTokenServer(t, func(w http.ResponseWriter, _ int64) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		})
		a := newTestAppAuth(t, pemKey, ts.apiBase())
		_, err := a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no token")
	})

	t.Run("missing expiry in response", func(t *testing.T) {
		ts := newTokenServer(t, func(w http.ResponseWriter, _ int64) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"token":"ghs_x"}`)
		})
		a := newTestAppAuth(t, pemKey, ts.apiBase())
		_, err := a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expiry")
	})

	t.Run("malformed json", func(t *testing.T) {
		ts := newTokenServer(t, func(w http.ResponseWriter, _ int64) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{not json`)
		})
		a := newTestAppAuth(t, pemKey, ts.apiBase())
		_, err := a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode")
	})
}

func TestAppAuthInstallationToken_ConcurrentMintsOnce(t *testing.T) {
	pemKey, _ := testRSAKeyPEM(t)
	ts := newTokenServer(t, okTokenHandler("ghs_shared", time.Now().Add(time.Hour)))
	a := newTestAppAuth(t, pemKey, ts.apiBase())

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			tok, err := a.installationToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if tok != "ghs_shared" {
				errs <- fmt.Errorf("unexpected token %q", tok)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), ts.mints.Load(), "concurrent callers must share a single mint")
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
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppInstallationID: 42, AppPrivateKey: pemKey},
			"https://api.github.com/", hc)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "request failed")
	})

	t.Run("malformed api base fails request build", func(t *testing.T) {
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppInstallationID: 42, AppPrivateKey: pemKey},
			"http://bad\x7fhost/", http.DefaultClient)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background())
		require.Error(t, err)
	})

	t.Run("body read error", func(t *testing.T) {
		hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusCreated,
				Body:       errReadCloser{},
				Header:     make(http.Header),
			}, nil
		})}
		a, err := newAppAuthFromOpts(Opts{AppID: 123, AppInstallationID: 42, AppPrivateKey: pemKey},
			"https://api.github.com/", hc)
		require.NoError(t, err)
		_, err = a.installationToken(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read")
	})
}

func TestNewServerClient(t *testing.T) {
	c := &client{API: defaultAPI, url: defaultURL}
	ctx := context.Background()

	t.Run("unconfigured falls back to user token", func(t *testing.T) {
		c.appAuth = nil
		gh, err := c.newServerClient(ctx, "user-token")
		require.NoError(t, err)
		assert.NotNil(t, gh)
	})

	t.Run("configured-but-broken fails loud, no user-token fallback", func(t *testing.T) {
		pemKey, _ := testRSAKeyPEM(t)
		ts := newTokenServer(t, func(w http.ResponseWriter, _ int64) {
			w.WriteHeader(http.StatusForbidden)
		})
		c.appAuth = newTestAppAuth(t, pemKey, ts.apiBase())
		gh, err := c.newServerClient(ctx, "user-token")
		require.Error(t, err)
		assert.Nil(t, gh, "must not return a user-token client when App auth is configured but failing")
		assert.Contains(t, err.Error(), "configured but failed")
	})

	t.Run("configured and working authenticates as app", func(t *testing.T) {
		pemKey, _ := testRSAKeyPEM(t)
		ts := newTokenServer(t, okTokenHandler("ghs_app", time.Now().Add(time.Hour)))
		c.appAuth = newTestAppAuth(t, pemKey, ts.apiBase())
		gh, err := c.newServerClient(ctx, "user-token")
		require.NoError(t, err)
		assert.NotNil(t, gh)
	})
}
