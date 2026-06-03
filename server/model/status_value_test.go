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

import (
	"errors"
	"testing"
)

func TestStatusValueNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   StatusValue
		want StatusValue
	}{
		// The #263 alias: British "cancelled" folds onto canonical "canceled".
		{"cancelled alias", "cancelled", StatusCanceled},
		// Canonical values pass through unchanged — Normalize never rewrites them.
		{"canonical canceled unchanged", StatusCanceled, StatusCanceled},
		{"success unchanged", StatusSuccess, StatusSuccess},
		{"killed unchanged", StatusKilled, StatusKilled},
		// Unknown values pass through unchanged so Validate can still reject them
		// (Normalize widens the input set only by documented aliases).
		{"unknown unchanged", "bogus", "bogus"},
		{"empty unchanged", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Normalize(); got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStatusValueNormalizeThenValidate(t *testing.T) {
	// The boundary contract: a normalized alias validates; a genuinely unknown
	// value still fails after normalization (no silent acceptance).
	if err := StatusValue("cancelled").Normalize().Validate(); err != nil {
		t.Fatalf("normalized cancelled should validate, got %v", err)
	}
	if got := StatusValue("cancelled").Normalize(); got != StatusCanceled {
		t.Fatalf("cancelled must normalize to canceled, got %q", got)
	}
	err := StatusValue("cancelledd").Normalize().Validate() // typo, not an alias
	if !errors.Is(err, ErrInvalidStatusValue) {
		t.Fatalf("unknown spelling must still be rejected, got %v", err)
	}
}

func TestStatusValueValidate(t *testing.T) {
	// Canonical values all validate; the alias is NOT a legal canonical value
	// (it is only accepted via Normalize at input boundaries).
	for _, s := range []StatusValue{
		StatusSkipped, StatusPending, StatusRunning, StatusSuccess, StatusPartial,
		StatusFailure, StatusKilled, StatusSuperseded, StatusCanceled, StatusError,
		StatusBlocked, StatusDeclined, StatusCreated,
	} {
		if err := s.Validate(); err != nil {
			t.Fatalf("canonical %q should validate, got %v", s, err)
		}
	}
	if err := StatusValue("cancelled").Validate(); !errors.Is(err, ErrInvalidStatusValue) {
		t.Fatalf("raw alias must not validate without Normalize, got %v", err)
	}
}
