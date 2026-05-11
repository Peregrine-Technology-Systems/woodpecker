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
	"errors"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// #209: WebSocket disconnect events at /api/ws-agent are the leading
// indicator for #190 mode-C kills. journalctl forensics on 2026-05-10
// found 195 events in 2.5h, 100% close-1006 ("abnormal closure"), and
// only ~1/195 had work in flight (the painful subset). This counter
// makes the underlying disconnect rate chartable in grafana so we
// don't have to re-grep journalctl by hand for the next incident.
//
// Cardinality is bounded: close_code is a small set of WebSocket spec
// codes, and hostname_prefix is normalized to a known-prefix bucket
// (see normalizeHostnamePrefix below).
var wsCloseTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "woodpecker",
	Name:      "ws_close_total",
	Help:      "Agent WebSocket disconnects observed at /api/ws-agent, by close code and hostname prefix (#209).",
}, []string{"close_code", "hostname_prefix"})

// Note: a "with-inflight" companion counter (the painful subset) was
// scoped out of this PR because surfacing the in-flight count from
// `cleanup()` requires touching `server/rpc/rpc.go::ReleaseAgentTasks`
// — large file with deep pre-existing coverage gaps. Tracked as a
// follow-up; in the meantime, correlate this counter with the
// `pipeline_dispatch_failures_total` from #192 and the new
// `kill_reason="agent_disconnect"` rows added by #202.

// recordWSClose extracts the close code from the ReadMessage error and
// records the disconnect against the bucketed hostname prefix. Falls
// back to "unknown" labels when the error doesn't carry a typed code
// (e.g. plain TCP RST without a websocket close frame).
func recordWSClose(err error, hostname string) {
	wsCloseTotal.WithLabelValues(extractCloseCode(err), normalizeHostnamePrefix(hostname)).Inc()
}

// extractCloseCode returns the WebSocket close code as a string (e.g.
// "1006") when the error is a *websocket.CloseError, else "unknown" for
// plain network errors that arrive without a close frame.
// errors.As handles both direct and wrapped *websocket.CloseError, so no
// separate type-assertion fallback is needed.
func extractCloseCode(err error) string {
	if err == nil {
		return "unknown"
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return wsCloseCodeString(ce.Code)
	}
	return "unknown"
}

func wsCloseCodeString(code int) string {
	switch code {
	case websocket.CloseNormalClosure:
		return "1000"
	case websocket.CloseGoingAway:
		return "1001"
	case websocket.CloseProtocolError:
		return "1002"
	case websocket.CloseUnsupportedData:
		return "1003"
	case websocket.CloseNoStatusReceived:
		return "1005"
	case websocket.CloseAbnormalClosure:
		return "1006"
	case websocket.CloseInvalidFramePayloadData:
		return "1007"
	case websocket.ClosePolicyViolation:
		return "1008"
	case websocket.CloseMessageTooBig:
		return "1009"
	case websocket.CloseMandatoryExtension:
		return "1010"
	case websocket.CloseInternalServerErr:
		return "1011"
	case websocket.CloseServiceRestart:
		return "1012"
	case websocket.CloseTryAgainLater:
		return "1013"
	case websocket.CloseTLSHandshake:
		return "1015"
	default:
		return "unknown"
	}
}

// normalizeHostnamePrefix buckets a hostname into a stable, low-cardinality
// label. Strategy:
//  1. Take the host part (everything before the first `.`) so FQDN
//     suffixes like `.us-east1-d.c.ci-runners-de.internal` don't bloat
//     cardinality.
//  2. Split on `-`. If the last segment is exactly 4 lowercase-alphanumeric
//     characters, treat it as a GCP random instance ID (e.g. `c9mp`, `lcfl`,
//     `wmmb` in `ci-spot-us-eas-c9mp`) and drop it. GCP's random suffix is
//     base-32 encoded and can be all-letters — the previous digit-check
//     silently leaked all-letter suffixes as per-instance label values (#214).
//  3. Rejoin and cap at 32 chars to prevent pathological inputs from
//     blowing the cardinality budget.
//
// Examples:
//
//	ci-spot-us-eas-c9mp.us-east1-d…internal → ci-spot-us-eas  (digit suffix)
//	ci-od-us-eas-lcfl.us-east1-b…internal   → ci-od-us-eas    (all-letter suffix, #214)
//	ci-od-us-cen-7zkl.us-central1-a…internal → ci-od-us-cen
//	integration-test-vm                      → integration-test-vm
//	some.fully.qualified.domain              → some
//	(empty)                                  → unknown
func normalizeHostnamePrefix(hostname string) string {
	if hostname == "" {
		return "unknown"
	}
	host := hostname
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i]
	}
	parts := strings.Split(host, "-")
	if len(parts) > 1 && isGCPInstanceSuffix(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	out := strings.Join(parts, "-")
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// isGCPInstanceSuffix returns true for exactly-4-char lowercase-alphanumeric
// strings — the format GCP uses for its random VM name suffix.
func isGCPInstanceSuffix(s string) bool {
	if len(s) != 4 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
