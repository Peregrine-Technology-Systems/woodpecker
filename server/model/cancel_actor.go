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

package model

import (
	"errors"
	"regexp"
)

// ErrInvalidCancelActor is returned for a declared cancel actor that is not a
// plain lowercase identifier.
var ErrInvalidCancelActor = errors.New("invalid cancel source: want 2-64 chars, lowercase letter first, then [a-z0-9._-]")

// cancelActorPattern is deliberately narrow. The value is written into a stored
// row and into log lines, so it must not be able to carry whitespace, control
// characters or anything that could forge a log entry. Go's `$` matches only at
// the very end of the text, so a trailing newline does not slip through.
var cancelActorPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)

// ParseCancelActor validates a caller-declared cancel actor (#388).
//
// An empty value means "not declared" and is not an error, which keeps callers
// that send nothing new working unchanged. A malformed value is REJECTED rather
// than normalised or dropped: lowercasing or trimming would store something the
// caller never sent, and dropping it would make an attributed cancel look
// unattributed — the exact ambiguity this field exists to remove.
func ParseCancelActor(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !cancelActorPattern.MatchString(raw) {
		return "", ErrInvalidCancelActor
	}
	return raw, nil
}
