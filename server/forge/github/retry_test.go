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
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-github/v84/github"
	"github.com/stretchr/testify/assert"

	forge_types "go.woodpecker-ci.org/woodpecker/v3/server/forge/types"
)

func resp(status int) *github.Response {
	return &github.Response{Response: &http.Response{StatusCode: status}}
}

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "dial tcp: i/o timeout" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

func TestIsRetryableForgeError(t *testing.T) {
	tests := []struct {
		name string
		resp *github.Response
		err  error
		want bool
	}{
		{"nil error is never retryable", resp(200), nil, false},
		{"caller context canceled is not a forge transient", nil, context.Canceled, false},
		{"http 500 is transient", resp(http.StatusInternalServerError), errors.New("boom"), true},
		{"http 502 is transient", resp(http.StatusBadGateway), errors.New("boom"), true},
		{"http 503 is transient", resp(http.StatusServiceUnavailable), errors.New("boom"), true},
		{"http 504 is transient", resp(http.StatusGatewayTimeout), errors.New("boom"), true},
		{"http 429 is transient", resp(http.StatusTooManyRequests), errors.New("boom"), true},
		{"http 400 is permanent", resp(http.StatusBadRequest), errors.New("boom"), false},
		{"http 401 is permanent", resp(http.StatusUnauthorized), errors.New("boom"), false},
		{"http 404 is permanent", resp(http.StatusNotFound), errors.New("boom"), false},
		{"http 422 is permanent", resp(http.StatusUnprocessableEntity), errors.New("boom"), false},
		{"deadline exceeded with no response is transient", nil, context.DeadlineExceeded, true},
		{"net error with no response is transient", nil, fakeNetErr{}, true},
		{"opaque error with no response is permanent", nil, errors.New("weird parse failure"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isRetryableForgeError(tt.resp, tt.err))
		})
	}
}

func TestIsRetryableForgeError_TypedRateLimit(t *testing.T) {
	// go-github surfaces rate-limit as a 403 with a typed error; classify by
	// type, not the raw 403 status (which is otherwise permanent).
	rle := &github.RateLimitError{Response: resp(http.StatusForbidden).Response}
	assert.True(t, isRetryableForgeError(resp(http.StatusForbidden), rle))

	are := &github.AbuseRateLimitError{Response: resp(http.StatusForbidden).Response}
	assert.True(t, isRetryableForgeError(resp(http.StatusForbidden), are))

	// A plain 403 with no rate-limit typing stays permanent.
	assert.False(t, isRetryableForgeError(resp(http.StatusForbidden), errors.New("forbidden")))
}

func TestRetryHookForgeCall_SucceedsFirstTry(t *testing.T) {
	calls := 0
	err := retryHookForgeCall(t.Context(), func() (*github.Response, error) {
		calls++
		return resp(200), nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 1, calls, "no retry on success")
}

func TestRetryHookForgeCall_RecoversAfterTransient(t *testing.T) {
	calls := 0
	err := retryHookForgeCall(t.Context(), func() (*github.Response, error) {
		calls++
		if calls < 2 {
			return resp(http.StatusBadGateway), errors.New("502")
		}
		return resp(200), nil
	})
	assert.NoError(t, err, "a transient 502 followed by success must not fail")
	assert.Equal(t, 2, calls)
}

func TestRetryHookForgeCall_PermanentErrorNotRetried(t *testing.T) {
	calls := 0
	sentinel := errors.New("malformed")
	err := retryHookForgeCall(t.Context(), func() (*github.Response, error) {
		calls++
		return resp(http.StatusUnprocessableEntity), sentinel
	})
	assert.ErrorIs(t, err, sentinel, "permanent error returned verbatim")
	assert.Equal(t, 1, calls, "permanent error is not retried")
	assert.NotErrorIs(t, err, &forge_types.ErrTransientForge{}, "permanent error must not be wrapped as transient")
}

func TestRetryHookForgeCall_ExhaustionWrapsAsTransient(t *testing.T) {
	calls := 0
	underlying := fmt.Errorf("upstream 503")
	err := retryHookForgeCall(t.Context(), func() (*github.Response, error) {
		calls++
		return resp(http.StatusServiceUnavailable), underlying
	})
	assert.Equal(t, hookForgeRetryAttempts, calls, "exhausts all attempts on a persistent transient")
	assert.ErrorIs(t, err, &forge_types.ErrTransientForge{}, "must be classifiable as transient by hook.go")
	assert.ErrorIs(t, err, underlying, "must still unwrap to the underlying cause")
}

func TestRetryHookForgeCall_ContextCanceledDuringBackoffAborts(t *testing.T) {
	// A transient failure on the first attempt enters the backoff wait; a
	// context that expires during that wait must abandon the retries promptly
	// rather than sleeping out the full backoff.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	start := time.Now()
	err := retryHookForgeCall(ctx, func() (*github.Response, error) {
		calls++
		return resp(http.StatusBadGateway), errors.New("502")
	})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, calls, "no further attempt after the backoff wait is cancelled")
	assert.Less(t, time.Since(start), hookForgeRetryBackoff, "must not sleep through the full backoff")
}

func TestRetryHookForgeCall_CanceledContextAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	err := retryHookForgeCall(ctx, func() (*github.Response, error) {
		calls++
		return resp(200), nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, calls, "a pre-cancelled context makes no forge call")
}

// interface assertion so the fake stays a net.Error.
var _ net.Error = fakeNetErr{}
