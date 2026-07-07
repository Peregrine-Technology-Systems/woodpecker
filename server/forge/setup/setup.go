package setup

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/forge"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/addon"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/bitbucket"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/bitbucketdatacenter"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/forgejo"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/gitea"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/github"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/gitlab"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

func Forge(forge *model.Forge) (forge.Forge, error) {
	switch forge.Type {
	case model.ForgeTypeAddon:
		return setupAddon(forge)
	case model.ForgeTypeGithub:
		return setupGitHub(forge)
	case model.ForgeTypeGitlab:
		return setupGitLab(forge)
	case model.ForgeTypeBitbucket:
		return setupBitbucket(forge)
	case model.ForgeTypeGitea:
		return setupGitea(forge)
	case model.ForgeTypeForgejo:
		return setupForgejo(forge)
	case model.ForgeTypeBitbucketDatacenter:
		return setupBitbucketDatacenter(forge)
	default:
		return nil, fmt.Errorf("forge not configured")
	}
}

func setupBitbucket(forge *model.Forge) (forge.Forge, error) {
	opts := &bitbucket.Opts{
		OAuthClientID:     forge.OAuthClientID,
		OAuthClientSecret: forge.OAuthClientSecret,
	}

	log.Debug().
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-client-secret-set", opts.OAuthClientSecret != "").
		Str("type", string(forge.Type)).
		Msg("setting up forge")
	return bitbucket.New(forge.ID, opts)
}

func setupGitea(forge *model.Forge) (forge.Forge, error) {
	serverURL, err := url.Parse(forge.URL)
	if err != nil {
		return nil, err
	}

	opts := gitea.Opts{
		URL:               strings.TrimRight(serverURL.String(), "/"),
		OAuthClientID:     forge.OAuthClientID,
		OAuthClientSecret: forge.OAuthClientSecret,
		SkipVerify:        forge.SkipVerify,
		OAuthHost:         forge.OAuthHost,
	}
	if len(opts.URL) == 0 {
		return nil, fmt.Errorf("WOODPECKER_GITEA_URL must be set")
	}
	log.Debug().
		Str("url", opts.URL).
		Str("oauth-host", opts.OAuthHost).
		Bool("skip-verify", opts.SkipVerify).
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-secret-id-set", opts.OAuthClientSecret != "").
		Str("type", string(forge.Type)).
		Msg("setting up forge")
	return gitea.New(forge.ID, opts)
}

func setupForgejo(forge *model.Forge) (forge.Forge, error) {
	server, err := url.Parse(forge.URL)
	if err != nil {
		return nil, err
	}

	opts := forgejo.Opts{
		URL:               strings.TrimRight(server.String(), "/"),
		OAuthClientID:     forge.OAuthClientID,
		OAuthClientSecret: forge.OAuthClientSecret,
		SkipVerify:        forge.SkipVerify,
		OAuth2URL:         forge.OAuthHost,
	}
	if len(opts.URL) == 0 {
		return nil, fmt.Errorf("WOODPECKER_FORGEJO_URL must be set")
	}
	log.Debug().
		Str("url", opts.URL).
		Str("oauth2-url", opts.OAuth2URL).
		Bool("skip-verify", opts.SkipVerify).
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-client-secret-set", opts.OAuthClientSecret != "").
		Str("type", string(forge.Type)).
		Msg("setting up forge")
	return forgejo.New(forge.ID, opts)
}

func setupGitLab(forge *model.Forge) (forge.Forge, error) {
	opts := gitlab.Opts{
		URL:               forge.URL,
		OAuthClientID:     forge.OAuthClientID,
		OAuthClientSecret: forge.OAuthClientSecret,
		SkipVerify:        forge.SkipVerify,
		OAuthHost:         forge.OAuthHost,
	}
	log.Debug().
		Str("url", opts.URL).
		Str("oauth-host", opts.OAuthHost).
		Bool("skip-verify", opts.SkipVerify).
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-client-secret-set", opts.OAuthClientSecret != "").
		Str("type", string(forge.Type)).
		Msg("setting up forge")
	return gitlab.New(forge.ID, opts)
}

func setupGitHub(forge *model.Forge) (forge.Forge, error) {
	// get additional config and be false by default
	mergeRef, _ := forge.AdditionalOptions["merge-ref"].(bool)
	publicOnly, _ := forge.AdditionalOptions["public-only"].(bool)

	// [pts] woodpecker#303: GitHub-App installation auth for server-initiated
	// forge calls. Read from env, not the forge DB record: the private key is
	// a secret and model.Forge.AdditionalOptions is exposed via the forge API
	// (json:"additional_options"), so persisting the key there would leak it.
	// Env is written by infra's deploy from GCP SM. All three unset =>
	// unchanged user-token behavior; a partial/invalid config fails loudly in
	// github.New.
	appID, err := parseGitHubAppInt("WOODPECKER_GITHUB_APP_ID")
	if err != nil {
		return nil, err
	}
	appInstallationID, err := parseGitHubAppInt("WOODPECKER_GITHUB_APP_INSTALLATION_ID")
	if err != nil {
		return nil, err
	}
	appKey, err := readGitHubAppKey()
	if err != nil {
		return nil, err
	}

	opts := github.Opts{
		URL:               forge.URL,
		OAuthClientID:     forge.OAuthClientID,
		OAuthClientSecret: forge.OAuthClientSecret,
		SkipVerify:        forge.SkipVerify,
		MergeRef:          mergeRef,
		OnlyPublic:        publicOnly,
		OAuthHost:         forge.OAuthHost,
		AppID:             appID,
		AppInstallationID: appInstallationID,
		AppPrivateKey:     appKey,
	}
	log.Debug().
		Str("url", opts.URL).
		Str("oauth-host", opts.OAuthHost).
		Bool("merge-ref", opts.MergeRef).
		Bool("only-public", opts.OnlyPublic).
		Bool("skip-verify", opts.SkipVerify).
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-client-secret-set", opts.OAuthClientSecret != "").
		Bool("app-auth-configured", opts.AppID != 0 && opts.AppInstallationID != 0 && len(opts.AppPrivateKey) > 0).
		Str("type", string(forge.Type)).
		Msg("setting up forge")
	return github.New(forge.ID, opts)
}

// [pts] woodpecker#303 GitHub-App config readers. Env-only (see setupGitHub).
// An unset var yields the zero value (=> unconfigured); a set-but-malformed var
// returns an error so a broken deploy fails loudly instead of silently reverting
// to user-PAT auth.

func parseGitHubAppInt(envVar string) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(envVar))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", envVar, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %d", envVar, n)
	}
	return n, nil
}

// readGitHubAppKey reads the App private key PEM from WOODPECKER_GITHUB_APP_KEY,
// or from the file at WOODPECKER_GITHUB_APP_KEY_FILE if that is set. Returns nil
// when neither is set (=> unconfigured).
func readGitHubAppKey() ([]byte, error) {
	if path := strings.TrimSpace(os.Getenv("WOODPECKER_GITHUB_APP_KEY_FILE")); path != "" {
		key, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("WOODPECKER_GITHUB_APP_KEY_FILE could not be read: %w", err)
		}
		return key, nil
	}
	if key := strings.TrimSpace(os.Getenv("WOODPECKER_GITHUB_APP_KEY")); key != "" {
		return []byte(key), nil
	}
	return nil, nil
}

func setupBitbucketDatacenter(forge *model.Forge) (forge.Forge, error) {
	gitUsername, ok := forge.AdditionalOptions["git-username"].(string)
	if !ok {
		return nil, fmt.Errorf("missing git-username")
	}
	gitPassword, ok := forge.AdditionalOptions["git-password"].(string)
	if !ok {
		return nil, fmt.Errorf("missing git-password")
	}

	enableProjectAdminScope, ok := forge.AdditionalOptions["oauth-enable-project-admin-scope"].(bool)
	if !ok {
		return nil, fmt.Errorf("incorrect type for oauth-enable-project-admin-scope value")
	}

	opts := bitbucketdatacenter.Opts{
		URL:                          forge.URL,
		OAuthClientID:                forge.OAuthClientID,
		OAuthClientSecret:            forge.OAuthClientSecret,
		Username:                     gitUsername,
		Password:                     gitPassword,
		OAuthHost:                    forge.OAuthHost,
		OAuthEnableProjectAdminScope: enableProjectAdminScope,
	}
	log.Debug().
		Str("url", opts.URL).
		Str("oauth-host", opts.OAuthHost).
		Bool("oauth-client-id-set", opts.OAuthClientID != "").
		Bool("oauth-client-secret-set", opts.OAuthClientSecret != "").
		Str("type", string(forge.Type)).
		Bool("oauth-enable-project-admin-scope", opts.OAuthEnableProjectAdminScope).
		Msg("setting up forge")
	return bitbucketdatacenter.New(forge.ID, opts)
}

func setupAddon(forge *model.Forge) (forge.Forge, error) {
	executable, ok := forge.AdditionalOptions["executable"].(string)
	if !ok {
		return nil, fmt.Errorf("missing addon executable")
	}

	log.Debug().Str("executable", executable).Msg("setting up forge")
	return addon.Load(executable)
}
