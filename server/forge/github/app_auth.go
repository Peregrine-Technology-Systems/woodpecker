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

// [pts] GitHub-App installation-token auth for server-initiated forge
// operations (woodpecker#303). Background API calls (config fetch, commit
// status, branch head, hook enrichment) draw on the App installation's own
// rate bucket instead of the activating user's shared PAT (user 101611),
// which repeatedly exhausted the 5000/hr limit and dropped webhooks
// (woodpecker#301/#308). User-initiated browsing (Repos/Teams/Org/Branches/
// PullRequests, OAuth login) deliberately stays on the user token.

package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v84/github"
)

const (
	// appJWTLifetime is how long the App JWT (used to mint installation
	// tokens) is valid. GitHub rejects App JWTs with an expiry more than
	// 10 minutes in the future; stay comfortably under that.
	appJWTLifetime = 9 * time.Minute
	// appJWTBackdate offsets iat into the past to tolerate clock skew
	// between this server and GitHub's auth servers.
	appJWTBackdate = 30 * time.Second
	// appTokenRefreshMargin refreshes a cached installation token this far
	// before its stated expiry, so an in-flight request never races the
	// boundary. Installation tokens live ~1h.
	appTokenRefreshMargin = 5 * time.Minute
	// maxAppTokenResponseBytes caps the token-endpoint response read.
	maxAppTokenResponseBytes = 1 << 20
)

// appAuth mints and caches GitHub-App installation access tokens. It is safe
// for concurrent use: hook processing fans out across goroutines, and all of
// them share one cached token guarded by mu.
type appAuth struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
	api            string // forge API base, trailing slash (e.g. https://api.github.com/)
	httpClient     *http.Client

	// now is injected so tests can control the clock; production uses
	// time.Now.
	now func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// newAppAuthFromOpts builds an appAuth from forge options. It returns
// (nil, nil) when App auth is unconfigured (all three fields empty) so the
// caller falls back to user-token auth — backward compatible. It returns an
// error when App auth is *partially* configured or the key is unparseable, so
// a misconfigured deploy fails loudly at startup rather than silently reverting
// to the shared PAT it was meant to replace.
func newAppAuthFromOpts(opts Opts, apiURL string, httpClient *http.Client) (*appAuth, error) {
	unset := opts.AppID == 0 && opts.AppInstallationID == 0 && len(opts.AppPrivateKey) == 0
	if unset {
		return nil, nil
	}
	if opts.AppID == 0 || opts.AppInstallationID == 0 || len(opts.AppPrivateKey) == 0 {
		return nil, fmt.Errorf("github app auth is partially configured: app-id, app-installation-id, and app-key must all be set together")
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM(opts.AppPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse github app private key: %w", err)
	}

	return &appAuth{
		appID:          opts.AppID,
		installationID: opts.AppInstallationID,
		privateKey:     key,
		api:            apiURL,
		httpClient:     httpClient,
		now:            time.Now,
	}, nil
}

// installationToken returns a valid installation access token, minting a fresh
// one only when the cache is empty or within the refresh margin of expiry.
func (a *appAuth) installationToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && a.now().Before(a.expiry.Add(-appTokenRefreshMargin)) {
		return a.token, nil
	}

	token, expiry, err := a.mintInstallationToken(ctx)
	if err != nil {
		// Do not serve a stale token past its refresh margin: a caller that
		// gets a soon-to-expire token would fail mid-request. Fail loud.
		return "", err
	}

	a.token = token
	a.expiry = expiry
	return token, nil
}

// mintInstallationToken performs the JWT -> installation-token exchange against
// the forge. It does not touch the cache; installationToken owns the lock.
func (a *appAuth) mintInstallationToken(ctx context.Context) (string, time.Time, error) {
	jwtToken, err := a.signAppJWT()
	if err != nil {
		return "", time.Time{}, err
	}

	url := fmt.Sprintf("%sapp/installations/%d/access_tokens", a.api, a.installationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app installation token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAppTokenResponseBytes))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read github app installation token response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("github app installation token request returned %d: %s", resp.StatusCode, string(body))
	}

	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to decode github app installation token response: %w", err)
	}
	if parsed.Token == "" {
		return "", time.Time{}, fmt.Errorf("github app installation token response contained no token")
	}
	if parsed.ExpiresAt.IsZero() {
		return "", time.Time{}, fmt.Errorf("github app installation token response contained no expiry")
	}

	return parsed.Token, parsed.ExpiresAt, nil
}

// signAppJWT builds and signs the short-lived App JWT used as the bearer for
// the installation-token exchange.
func (a *appAuth) signAppJWT() (string, error) {
	now := a.now()
	claims := jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-appJWTBackdate)),
		ExpiresAt: jwt.NewNumericDate(now.Add(appJWTLifetime)),
		Issuer:    strconv.FormatInt(a.appID, 10),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(a.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign github app jwt: %w", err)
	}
	return signed, nil
}

// newServerClient returns a GitHub client for a server-initiated (background)
// operation. When App auth is configured it authenticates as the App
// installation, drawing on the App's own rate bucket. When App auth is
// unconfigured it falls back to fallbackToken (the per-call-site user token),
// preserving exactly the pre-App behavior.
//
// When App auth *is* configured but minting fails, it returns the error rather
// than falling back to the user token: a silent fallback would re-exhaust the
// shared PAT this path exists to relieve and hide the misconfiguration.
func (c *client) newServerClient(ctx context.Context, fallbackToken string) (*github.Client, error) {
	if c.appAuth == nil {
		return c.newClientToken(ctx, fallbackToken), nil
	}
	token, err := c.appAuth.installationToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("github app auth is configured but failed to mint an installation token: %w", err)
	}
	return c.newClientToken(ctx, token), nil
}
