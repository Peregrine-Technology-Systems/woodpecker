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

package model

// statusAliases maps accepted non-canonical spellings to their canonical
// StatusValue. The British "cancelled" (double-l) is a common consumer
// muscle-memory spelling, but Woodpecker's canonical wire/stored value is the
// American "canceled" (single-l) — see StatusCanceled in const.go. Accept the
// alias on INPUT only (see Normalize): emitted and stored values stay
// canonical so existing consumers matching "canceled" never see a spelling
// change. (#263)
var statusAliases = map[StatusValue]StatusValue{
	"cancelled": StatusCanceled,
}

// Normalize folds an accepted alias spelling onto its canonical StatusValue.
// Values that are already canonical (or genuinely unknown) are returned
// unchanged — Validate still rejects unknown values after normalization, so
// this only ever widens the accepted INPUT set by the documented aliases,
// never the legal OUTPUT set. Call at input boundaries (e.g. the ?status=
// pipeline filter) before Validate. (#263)
func (s StatusValue) Normalize() StatusValue {
	if canonical, ok := statusAliases[s]; ok {
		return canonical
	}
	return s
}
