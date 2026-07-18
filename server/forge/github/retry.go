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

package github

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/google/go-github/v84/github"

	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
)

const (
	// hookForgeRetryAttempts bounds retries of the synchronous forge-API calls
	// made while parsing an inbound webhook (Hook -> loadChangedFilesFrom* /
	// getTagCommitSHA). These run inside the webhook request, which the forge
	// abandons after ~10s, so the total retry budget is deliberately small.
	hookForgeRetryAttempts = 3
	// hookForgeRetryBackoff is the base linear backoff between hook-parse
	// retries (attempt N waits N*backoff), keeping the worst-case added delay
	// well under the forge's webhook-delivery timeout.
	hookForgeRetryBackoff = 200 * time.Millisecond
)

// retryHookForgeCall runs fn — a single go-github API call — up to
// hookForgeRetryAttempts times, retrying only transient forge failures (HTTP
// 5xx, 429/rate-limit, and network/timeout errors). On a permanent error (a
// non-429 4xx, or a malformed response) it returns immediately without
// retrying. When a transient error survives every attempt it is wrapped in
// forge_types.ErrTransientForge so the caller can translate it into a retryable
// HTTP status instead of a permanent one that the forge will never redeliver
// (woodpecker#321). fn must publish the API call's results into the caller's
// scope and return the *github.Response (for status classification) and error.
func retryHookForgeCall(ctx context.Context, fn func() (*github.Response, error)) error {
	var lastErr error
	for attempt := 0; attempt < hookForgeRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(hookForgeRetryBackoff * time.Duration(attempt)):
			}
		}

		resp, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableForgeError(resp, err) {
			return err
		}
	}
	return &forge_types.ErrTransientForge{Err: lastErr}
}

// isRetryableForgeError classifies a go-github call failure as transient
// (worth retrying / eventually surfacing as ErrTransientForge) or permanent.
// Transient: HTTP 5xx, HTTP 429, go-github's typed primary/secondary
// rate-limit errors, and transport-level failures (timeout, connection reset,
// DNS) that carry no HTTP response. Permanent: any other explicit 4xx, and a
// caller-cancelled context (which is not a forge fault).
func isRetryableForgeError(resp *github.Response, err error) bool {
	if err == nil {
		return false
	}

	// A cancelled caller context is not a forge transient — abort, don't retry.
	if errors.Is(err, context.Canceled) {
		return false
	}

	// go-github's typed rate-limit errors (primary and secondary/abuse) can
	// arrive as 403 with a rate-limit body, so classify them before the raw
	// status code below.
	var rle *github.RateLimitError
	var are *github.AbuseRateLimitError
	if errors.As(err, &rle) || errors.As(err, &are) {
		return true
	}

	if resp != nil {
		switch {
		case resp.StatusCode >= 500:
			return true
		case resp.StatusCode == http.StatusTooManyRequests:
			return true
		case resp.StatusCode >= 400:
			// Any other explicit 4xx is a permanent client error.
			return false
		}
	}

	// No HTTP response => a transport-level failure before the request
	// completed (timeout, reset, DNS). Treat these as transient.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
