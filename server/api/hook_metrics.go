// Copyright 2026 Woodpecker Authors
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

package api

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// #191: Two counters distinguish "GitHub never sent the webhook" from "we
// received it and dropped it" — without this we can only tell that a
// pipeline didn't get created. The receive counter increments at the top
// of PostHook before any validation; the drop counter increments at each
// early-return with a reason label drawn from the existing rejection
// branches. Together they let ops cross-correlate against the forge's
// outbound delivery log to localize where a missing pipeline died.
var (
	webhooksReceived = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "webhooks_received_total",
		Help:      "Webhooks received by /api/hook, regardless of outcome. Cross-correlate with forge outbound logs to detect upstream-drop vs server-drop (#190 mode A / #191).",
	}, []string{"source"})

	webhooksDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "woodpecker",
		Name:      "webhooks_dropped_total",
		Help:      "Webhooks rejected by /api/hook before pipeline creation, by reason. Reasons enumerated at the rejection sites in hook.go::PostHook (#191).",
	}, []string{"reason"})
)

// detectWebhookSource sniffs the request headers to identify the forge
// type. The forge's repo-driver isn't resolved until later in PostHook
// (and may fail to resolve at all on an early reject), so we derive the
// source from headers each forge already sends. Returns "unknown" when no
// header matches — those rows are still useful as a "stuck integration"
// signal.
func detectWebhookSource(r *http.Request) string {
	switch {
	case r.Header.Get("X-GitHub-Event") != "":
		return "github"
	case r.Header.Get("X-Gitea-Event") != "":
		return "gitea"
	case r.Header.Get("X-Forgejo-Event") != "":
		return "forgejo"
	case r.Header.Get("X-Gitlab-Event") != "":
		return "gitlab"
	case r.Header.Get("X-Event-Key") != "":
		return "bitbucket"
	default:
		return "unknown"
	}
}
