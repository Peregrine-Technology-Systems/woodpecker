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
//
// The installation is resolved *per repo owner* (GET /repos/{owner}/{repo}/
// installation), not from a single fixed id: d3ci42 serves repos under more
// than one GitHub account (the Peregrine org + a personal account), each a
// distinct App installation. A fixed id would hard-drop the other account's
// hooks. Tokens are cached per installation id; owner->installation is cached
// so the resolve happens once per account.

package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v84/github"
)

const (
	// appJWTLifetime is how long the App JWT (used to mint installation
	// tokens and resolve installations) is valid. GitHub rejects App JWTs
	// with an expiry more than 10 minutes in the future; stay under that.
	appJWTLifetime = 9 * time.Minute
	// appJWTBackdate offsets iat into the past to tolerate clock skew
	// between this server and GitHub's auth servers.
	appJWTBackdate = 30 * time.Second
	// appTokenRefreshMargin refreshes a cached installation token this far
	// before its stated expiry, so an in-flight request never races the
	// boundary. Installation tokens live ~1h.
	appTokenRefreshMargin = 5 * time.Minute
	// maxAppResponseBytes caps the App API response reads.
	maxAppResponseBytes = 1 << 20
)

// appAuth mints and caches GitHub-App installation access tokens, resolving
// the installation per repo owner. It is safe for concurrent use: hook
// processing fans out across goroutines that share the caches under mu.
type appAuth struct {
	appID      int64
	privateKey *rsa.PrivateKey
	api        string // forge API base, trailing slash (e.g. https://api.github.com/)
	httpClient *http.Client

	// now is injected so tests can control the clock; production uses
	// time.Now.
	now func() time.Time

	mu sync.Mutex
	// installations caches owner (lowercased) -> installation id. One App
	// has at most one installation per account, so this never goes stale
	// for the lifetime of the process.
	installations map[string]int64
	// tokens caches installation id -> the current installation access
	// token and its expiry.
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token  string
	expiry time.Time
}

// newAppAuthFromOpts builds an appAuth from forge options. It returns
// (nil, nil) when App auth is unconfigured (both fields empty) so the caller
// falls back to user-token auth — backward compatible. It returns an error
// when App auth is *partially* configured or the key is unparseable, so a
// misconfigured deploy fails loudly at startup rather than silently reverting
// to the shared PAT it was meant to replace.
func newAppAuthFromOpts(opts Opts, apiURL string, httpClient *http.Client) (*appAuth, error) {
	unset := opts.AppID == 0 && len(opts.AppPrivateKey) == 0
	if unset {
		return nil, nil
	}
	if opts.AppID == 0 || len(opts.AppPrivateKey) == 0 {
		return nil, fmt.Errorf("github app auth is partially configured: app-id and app-key must both be set together")
	}

	key, err := jwt.ParseRSAPrivateKeyFromPEM(opts.AppPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse github app private key: %w", err)
	}

	return &appAuth{
		appID:         opts.AppID,
		privateKey:    key,
		api:           apiURL,
		httpClient:    httpClient,
		now:           time.Now,
		installations: make(map[string]int64),
		tokens:        make(map[int64]cachedToken),
	}, nil
}

// installationToken returns a valid installation access token for the App's
// installation on owner's account, resolving the installation (once per owner)
// and minting a token (once per ~1h per installation) as needed.
func (a *appAuth) installationToken(ctx context.Context, owner, repo string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	installationID, err := a.resolveInstallationIDLocked(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	return a.tokenForInstallationLocked(ctx, installationID)
}

// resolveInstallationIDLocked returns the App installation id for owner,
// caching by owner. Caller holds a.mu.
func (a *appAuth) resolveInstallationIDLocked(ctx context.Context, owner, repo string) (int64, error) {
	key := strings.ToLower(owner)
	if id, ok := a.installations[key]; ok {
		return id, nil
	}

	url := fmt.Sprintf("%srepos/%s/%s/installation", a.api, owner, repo)
	body, status, err := a.appJWTRequest(ctx, http.MethodGet, url)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("github app installation lookup for %q returned %d: %s", owner, status, string(body))
	}

	var parsed struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("failed to decode github app installation lookup response: %w", err)
	}
	if parsed.ID == 0 {
		return 0, fmt.Errorf("github app installation lookup for %q returned no installation id", owner)
	}

	a.installations[key] = parsed.ID
	return parsed.ID, nil
}

// tokenForInstallationLocked returns a valid access token for installationID,
// minting a fresh one only when the cache is empty or within the refresh
// margin of expiry. Caller holds a.mu.
func (a *appAuth) tokenForInstallationLocked(ctx context.Context, installationID int64) (string, error) {
	if ct, ok := a.tokens[installationID]; ok && a.now().Before(ct.expiry.Add(-appTokenRefreshMargin)) {
		return ct.token, nil
	}

	token, expiry, err := a.mintInstallationToken(ctx, installationID)
	if err != nil {
		// Do not serve a stale token past its refresh margin: a caller that
		// gets a soon-to-expire token would fail mid-request. Fail loud.
		return "", err
	}

	a.tokens[installationID] = cachedToken{token: token, expiry: expiry}
	return token, nil
}

// mintInstallationToken performs the JWT -> installation-token exchange.
func (a *appAuth) mintInstallationToken(ctx context.Context, installationID int64) (string, time.Time, error) {
	url := fmt.Sprintf("%sapp/installations/%d/access_tokens", a.api, installationID)
	body, status, err := a.appJWTRequest(ctx, http.MethodPost, url)
	if err != nil {
		return "", time.Time{}, err
	}
	if status != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("github app installation token request returned %d: %s", status, string(body))
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

// appJWTRequest performs an App-JWT-authenticated request against the forge
// App API and returns the response body and status. Used for both installation
// resolution (GET) and token minting (POST).
func (a *appAuth) appJWTRequest(ctx context.Context, method, url string) ([]byte, int, error) {
	jwtToken, err := a.signAppJWT()
	if err != nil {
		return nil, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, method, url, http.NoBody)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("github app api request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAppResponseBytes))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read github app api response: %w", err)
	}
	return body, resp.StatusCode, nil
}

// signAppJWT builds and signs the short-lived App JWT used as the bearer for
// installation resolution and the token exchange.
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
// operation on the given repo. When App auth is configured it authenticates as
// the App installation on the repo owner's account, drawing on the App's own
// rate bucket. When App auth is unconfigured it falls back to fallbackToken
// (the per-call-site user token), preserving exactly the pre-App behavior.
//
// When App auth *is* configured but resolving/minting fails, it returns the
// error rather than falling back to the user token: a silent fallback would
// re-exhaust the shared PAT this path exists to relieve and hide the breakage.
func (c *client) newServerClient(ctx context.Context, owner, repo, fallbackToken string) (*github.Client, error) {
	if c.appAuth == nil {
		return c.newClientToken(ctx, fallbackToken), nil
	}
	token, err := c.appAuth.installationToken(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("github app auth is configured but failed to get an installation token for %q: %w", owner, err)
	}
	return c.newClientToken(ctx, token), nil
}
