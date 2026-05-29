// Copyright 2022 Woodpecker Authors
// Copyright 2018 Drone.IO Inc.
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

package main

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/tink-crypto/tink-go/v2/subtle/random"
	"github.com/urfave/cli/v3"

	"go.woodpecker-ci.org/woodpecker/v3/server"
	"go.woodpecker-ci.org/woodpecker/v3/server/api"
	"go.woodpecker-ci.org/woodpecker/v3/server/cache"
	"go.woodpecker-ci.org/woodpecker/v3/server/forge/setup"
	"go.woodpecker-ci.org/woodpecker/v3/server/logdrain"
	"go.woodpecker-ci.org/woodpecker/v3/server/logging"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	"go.woodpecker-ci.org/woodpecker/v3/server/pipeline"
	"go.woodpecker-ci.org/woodpecker/v3/server/pubsub"
	"go.woodpecker-ci.org/woodpecker/v3/server/queue"
	woodpeckerGrpc "go.woodpecker-ci.org/woodpecker/v3/server/rpc"
	"go.woodpecker-ci.org/woodpecker/v3/server/services"
	logService "go.woodpecker-ci.org/woodpecker/v3/server/services/log"
	"go.woodpecker-ci.org/woodpecker/v3/server/services/log/addon"
	"go.woodpecker-ci.org/woodpecker/v3/server/services/log/file"
	"go.woodpecker-ci.org/woodpecker/v3/server/services/permissions"
	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/datastore"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

const (
	queueInfoRefreshInterval = 500 * time.Millisecond
	storeInfoRefreshInterval = 10 * time.Second
)

func setupStore(ctx context.Context, c *cli.Command) (store.Store, error) {
	datasource := c.String("db-datasource")
	driver := c.String("db-driver")
	xorm := store.XORM{
		Log:             c.Bool("db-log"),
		ShowSQL:         c.Bool("db-log-sql"),
		MaxOpenConns:    c.Int("db-max-open-connections"),
		MaxIdleConns:    c.Int("db-max-idle-connections"),
		ConnMaxLifetime: c.Duration("db-max-connection-timeout"),
	}

	if driver == "sqlite3" {
		if datastore.SupportedDriver("sqlite3") {
			log.Debug().Msg("server has sqlite3 support")
		} else {
			log.Debug().Msg("server was built without sqlite3 support!")
		}
	}

	if !datastore.SupportedDriver(driver) {
		return nil, fmt.Errorf("database driver '%s' not supported", driver)
	}

	if driver == "sqlite3" {
		if err := checkSqliteFileExist(datasource); err != nil {
			return nil, fmt.Errorf("check sqlite file: %w", err)
		}
	}

	opts := &store.Opts{
		Driver: driver,
		Config: datasource,
		XORM:   xorm,
	}
	log.Debug().Str("driver", driver).Any("xorm", xorm).Msg("setting up datastore")
	store, err := datastore.NewEngine(opts)
	if err != nil {
		return nil, fmt.Errorf("could not open datastore: %w", err)
	}

	if err = store.Ping(); err != nil {
		return nil, err
	}

	if err := store.Migrate(ctx, c.Bool("migrations-allow-long")); err != nil {
		return nil, fmt.Errorf("could not migrate datastore: %w", err)
	}

	datastore.RunStartupChecks(ctx, store)

	return store, nil
}

func checkSqliteFileExist(dsn string) error {
	path := sqliteFilePathFromDSN(dsn)
	_, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		log.Warn().Msgf("no sqlite file found at '%s', will create one (dsn '%s')", path, dsn)
		return nil
	}
	return err
}

// sqliteFilePathFromDSN strips the optional "file:" scheme prefix and any
// query-string parameters (e.g. "?_busy_timeout=5000&_journal_mode=WAL") from
// the DSN, leaving just the on-disk path so os.Stat can find the actual file.
// Without this, an existing DB at "/var/lib/woodpecker/woodpecker.sqlite"
// stat'd as "file:/var/lib/woodpecker/woodpecker.sqlite?_busy_timeout=..."
// returns ENOENT and emits a misleading "no sqlite file found" warning on
// every server restart (fork#41).
func sqliteFilePathFromDSN(dsn string) string {
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
	return path
}

func setupQueue(ctx context.Context, s store.Store) (queue.Queue, error) {
	return queue.New(ctx, queue.Config{
		Backend: queue.TypeMemory,
		Store:   s,
	})
}

func setupMembershipService(_ context.Context, _store store.Store) cache.MembershipService {
	return cache.NewMembershipService(_store)
}

func setupLogStore(c *cli.Command, s store.Store) (logService.Service, error) {
	switch c.String("log-store") {
	case "file":
		return file.NewLogStore(c.String("log-store-file-path"))
	case "addon":
		return addon.Load(c.String("log-store-file-path"))
	default:
		return s, nil
	}
}

const jwtSecretID = "jwt-secret"

func setupJWTSecret(_store store.Store) (string, error) {
	jwtSecret, err := _store.ServerConfigGet(jwtSecretID)
	if errors.Is(err, types.ErrRecordNotExist) {
		jwtSecret := base32.StdEncoding.EncodeToString(
			random.GetRandomBytes(32),
		)
		err = _store.ServerConfigSet(jwtSecretID, jwtSecret)
		if err != nil {
			return "", err
		}
		log.Debug().Msg("created jwt secret")
		return jwtSecret, nil
	}

	if err != nil {
		return "", err
	}

	return jwtSecret, nil
}

func setupEvilGlobals(ctx context.Context, c *cli.Command, s store.Store) (err error) {
	// services
	server.Config.Services.Logs = logging.New()
	server.Config.Services.Pubsub = pubsub.New()
	server.Config.Services.Membership = setupMembershipService(ctx, s)
	server.Config.Services.Queue, err = setupQueue(ctx, s)
	if err != nil {
		return fmt.Errorf("could not setup queue: %w", err)
	}
	server.Config.Services.Manager, err = services.NewManager(c, s, setup.Forge)
	if err != nil {
		return fmt.Errorf("could not setup service manager: %w", err)
	}
	server.Config.Services.LogStore, err = setupLogStore(c, s)
	if err != nil {
		return fmt.Errorf("could not setup log store: %w", err)
	}

	// #233: best-effort step-log drain to GCP Cloud Logging. New never errors —
	// an unset project or unavailable ADC yields a disabled no-op (logged INFO).
	server.Config.Services.LogDrain = logdrain.New(ctx, c.String("log-drain-gcp-project"), c.String("log-drain-gcp-log-name"))

	// #891 / #176 / #188: Reconcile orphaned "running" pipelines.
	// The reconciler asks the queue which pipelines have active tasks
	// (Pending / Running / WaitingOnDeps) and only kills the rest.
	// Heartbeating agents keep their task in q.Running via Extend(); a
	// disconnected agent's task is moved to q.Pending by
	// resubmitExpiredPipelines — either way, the queue is the source of
	// truth for "is this pipeline making progress." This avoids both
	// the original #170 "kill any running pipeline" misfire AND the
	// #176 grace-period bandaid that wasn't long enough for normal
	// pts-build runtimes (5-15 min).
	pipeline.ReconcileOrphanedAtStartup(s, server.Config.Services.Queue)

	reconciler := pipeline.NewOrphanReconciler(server.Config.Services.Queue)
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				reconciler.Tick(s)
			case <-ctx.Done():
				return
			}
		}
	}()

	// WebSocket agent transport (#474) — shares business logic with gRPC
	wsAgentRPC := woodpeckerGrpc.NewRPC(
		server.Config.Services.Queue,
		server.Config.Services.Logs,
		server.Config.Services.Pubsub,
		s,
	)
	server.Config.Services.WSAgentRPC = wsAgentRPC

	// #243/#246: owner-liveness reclaim. Each dispatch tick the queue asks
	// api.IsAgentKnownDisconnected whether a running task's owning agent has been
	// POSITIVELY observed as gone (its WS reconnect grace expired); only then does
	// it fire this reclaim to release the stranded task(s) immediately instead of
	// waiting the 15-minute TaskTimeout lease. The predicate fails safe: a
	// gRPC/local-backend agent that never registers through the WS path is never
	// known-dead, so its in-flight steps are never killed at t+0s (#246).
	server.Config.Services.Queue.SetAgentReclaimFn(
		api.IsAgentKnownDisconnected,
		func(agentID int64) { api.ReclaimAgentTasks(agentID, wsAgentRPC) },
	)

	// plugins
	if err := setupPlugins(ctx, c); err != nil {
		return fmt.Errorf("could not setup plugins: %w", err)
	}

	// agents
	server.Config.Agent.DisableUserRegisteredAgentRegistration = c.Bool("disable-user-agent-registration")

	// authentication
	server.Config.Pipeline.AuthenticatePublicRepos = c.Bool("authenticate-public-repos")

	// Pull requests
	server.Config.Pipeline.DefaultAllowPullRequests = c.Bool("default-allow-pull-requests")

	// Approval mode
	approvalMode := model.ApprovalMode(c.String("default-approval-mode"))
	if !approvalMode.Valid() {
		return fmt.Errorf("approval mode %s is not valid", approvalMode)
	}
	server.Config.Pipeline.DefaultApprovalMode = approvalMode

	// Cloning
	server.Config.Pipeline.DefaultClonePlugin = c.String("default-clone-plugin")
	server.Config.Pipeline.TrustedClonePlugins = c.StringSlice("plugins-trusted-clone")
	server.Config.Pipeline.TrustedClonePlugins = append(server.Config.Pipeline.TrustedClonePlugins, server.Config.Pipeline.DefaultClonePlugin)

	// Execution
	_events := c.StringSlice("default-cancel-previous-pipeline-events")
	events := make([]model.WebhookEvent, 0, len(_events))
	for _, v := range _events {
		e := model.WebhookEvent(v)
		if err := e.Validate(); err != nil {
			return err
		}
		events = append(events, e)
	}
	server.Config.Pipeline.DefaultCancelPreviousPipelineEvents = events
	server.Config.Pipeline.DefaultTimeout = c.Int64("default-pipeline-timeout")
	server.Config.Pipeline.MaxTimeout = c.Int64("max-pipeline-timeout")

	_labels := c.StringSlice("default-workflow-labels")
	labels := make(map[string]string, len(_labels))
	for _, v := range _labels {
		name, value, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("invalid label filter: %s", v)
		}
		labels[name] = value
	}
	server.Config.Pipeline.DefaultWorkflowLabels = labels

	// backend options for pipeline compiler
	server.Config.Pipeline.Proxy.No = c.String("backend-no-proxy")
	server.Config.Pipeline.Proxy.HTTP = c.String("backend-http-proxy")
	server.Config.Pipeline.Proxy.HTTPS = c.String("backend-https-proxy")

	// server configuration
	server.Config.Server.JWTSecret, err = setupJWTSecret(s)
	if err != nil {
		return fmt.Errorf("could not setup jwt secret: %w", err)
	}
	server.Config.Server.Cert = c.String("server-cert")
	server.Config.Server.Key = c.String("server-key")
	server.Config.Server.AgentToken = c.String("agent-secret")
	serverHost := strings.TrimSuffix(c.String("server-host"), "/")
	server.Config.Server.Host = serverHost
	if c.IsSet("server-webhook-host") {
		server.Config.Server.WebhookHost = c.String("server-webhook-host")
	} else {
		server.Config.Server.WebhookHost = serverHost
	}
	server.Config.Server.OAuthHost = serverHost
	server.Config.Server.Port = c.String("server-addr")
	server.Config.Server.PortTLS = c.String("server-addr-tls")
	server.Config.Server.StatusContext = c.String("status-context")
	server.Config.Server.StatusContextFormat = c.String("status-context-format")
	server.Config.Server.SessionExpires = c.Duration("session-expires")
	u, _ := url.Parse(server.Config.Server.Host)
	rootPath := strings.TrimSuffix(u.Path, "/")
	if rootPath != "" && !strings.HasPrefix(rootPath, "/") {
		rootPath = "/" + rootPath
	}
	server.Config.Server.RootPath = rootPath
	server.Config.Server.CustomCSSFile = strings.TrimSpace(c.String("custom-css-file"))
	server.Config.Server.CustomJsFile = strings.TrimSpace(c.String("custom-js-file"))
	server.Config.Pipeline.Networks = c.StringSlice("network")
	server.Config.Pipeline.Volumes = c.StringSlice("volume")
	server.Config.WebUI.EnableSwagger = c.Bool("enable-swagger")
	server.Config.WebUI.SkipVersionCheck = c.Bool("skip-version-check")
	server.Config.WebUI.MaxPipelineLogLineCount = c.Uint("max-pipeline-log-line-count")
	server.Config.Pipeline.PrivilegedPlugins = c.StringSlice("plugins-privileged")

	// prometheus
	server.Config.Prometheus.AuthToken = c.String("prometheus-auth-token")

	// permissions
	server.Config.Permissions.Open = c.Bool("open")
	server.Config.Permissions.Admins = permissions.NewAdmins(c.StringSlice("admin"))
	server.Config.Permissions.Orgs = permissions.NewOrgs(c.StringSlice("orgs"))
	server.Config.Permissions.OwnersAllowlist = permissions.NewOwnersAllowlist(c.StringSlice("repo-owners"))
	return nil
}
