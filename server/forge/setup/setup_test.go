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

package setup

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// [pts] woodpecker#303 — coverage for the GitHub-App env-config path added to
// setupGitHub. The rest of setup.go is pre-existing forge-wiring glue and is
// covered by the documented COV_WIRING exemption; these tests own the new logic.

func testAppKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func githubForge() *model.Forge {
	return &model.Forge{Type: model.ForgeTypeGithub, URL: "https://github.com"}
}

func TestSetupGitHub_AppAuthUnconfigured(t *testing.T) {
	// No App env set → backward-compatible user-token forge, no error.
	f, err := setupGitHub(githubForge())
	require.NoError(t, err)
	assert.NotNil(t, f)
}

func TestSetupGitHub_AppAuthValid(t *testing.T) {
	t.Setenv("WOODPECKER_GITHUB_APP_ID", "123")
	t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "42")
	t.Setenv("WOODPECKER_GITHUB_APP_KEY", string(testAppKeyPEM(t)))

	f, err := setupGitHub(githubForge())
	require.NoError(t, err)
	assert.NotNil(t, f)
}

func TestSetupGitHub_AppAuthKeyFromFile(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	require.NoError(t, os.WriteFile(keyPath, testAppKeyPEM(t), 0o600))

	t.Setenv("WOODPECKER_GITHUB_APP_ID", "123")
	t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "42")
	t.Setenv("WOODPECKER_GITHUB_APP_KEY_FILE", keyPath)

	f, err := setupGitHub(githubForge())
	require.NoError(t, err)
	assert.NotNil(t, f)
}

func TestSetupGitHub_AppAuthFailsLoud(t *testing.T) {
	t.Run("partial config (id without key)", func(t *testing.T) {
		t.Setenv("WOODPECKER_GITHUB_APP_ID", "123")
		t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "42")
		_, err := setupGitHub(githubForge())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "partially configured")
	})

	t.Run("non-integer app id", func(t *testing.T) {
		t.Setenv("WOODPECKER_GITHUB_APP_ID", "not-a-number")
		_, err := setupGitHub(githubForge())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WOODPECKER_GITHUB_APP_ID")
	})

	t.Run("non-positive installation id", func(t *testing.T) {
		t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "0")
		_, err := setupGitHub(githubForge())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})

	t.Run("unreadable key file", func(t *testing.T) {
		t.Setenv("WOODPECKER_GITHUB_APP_ID", "123")
		t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "42")
		t.Setenv("WOODPECKER_GITHUB_APP_KEY_FILE", "/nonexistent/path/app.pem")
		_, err := setupGitHub(githubForge())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "WOODPECKER_GITHUB_APP_KEY_FILE")
	})

	t.Run("invalid key content", func(t *testing.T) {
		t.Setenv("WOODPECKER_GITHUB_APP_ID", "123")
		t.Setenv("WOODPECKER_GITHUB_APP_INSTALLATION_ID", "42")
		t.Setenv("WOODPECKER_GITHUB_APP_KEY", "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----")
		_, err := setupGitHub(githubForge())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "private key")
	})
}
