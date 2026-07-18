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

package types

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotImplemented = errors.New("not implemented")
	ErrRepoNotFound   = errors.New("repo not found")
)

type ErrIgnoreEvent struct {
	Event  string
	Reason string
}

func (err *ErrIgnoreEvent) Error() string {
	if err.Reason != "" {
		return fmt.Sprintf("explicit ignored event '%s', reason: %s", err.Event, err.Reason)
	}
	return fmt.Sprintf("explicit ignored event '%s'", err.Event)
}

func (*ErrIgnoreEvent) Is(target error) bool {
	_, ok := target.(*ErrIgnoreEvent)
	return ok
}

// ErrTransientForge wraps a forge-API failure that is transient (HTTP 5xx,
// rate-limit, or a network/timeout error) and survived a forge adapter's
// bounded retries. Callers translate it into a retryable response (e.g. an
// inbound webhook handler returning HTTP 503 instead of a permanent 400) so the
// forge's own delivery-retry mechanism can redeliver later, rather than
// permanently stranding a CI trigger on a momentary upstream blip
// (woodpecker#321).
type ErrTransientForge struct {
	Err error
}

func (e *ErrTransientForge) Error() string {
	if e.Err != nil {
		return "transient forge error: " + e.Err.Error()
	}
	return "transient forge error"
}

func (e *ErrTransientForge) Unwrap() error { return e.Err }

func (*ErrTransientForge) Is(target error) bool {
	_, ok := target.(*ErrTransientForge)
	return ok
}

type ErrConfigNotFound struct {
	Configs []string
}

func (m *ErrConfigNotFound) Error() string {
	return fmt.Sprintf("configs not found: %s", strings.Join(m.Configs, ", "))
}

func (*ErrConfigNotFound) Is(target error) bool {
	_, ok := target.(*ErrConfigNotFound)
	return ok
}
