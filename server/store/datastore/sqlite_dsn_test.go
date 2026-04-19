// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package datastore

import (
	"net/url"
	"strings"
	"testing"
)

func TestApplySqliteDefaults(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantKV  map[string]string
		wantRaw string
	}{
		{
			name: "plain path adds all defaults",
			in:   "/var/lib/woodpecker/woodpecker.sqlite",
			wantKV: map[string]string{
				"_busy_timeout": "30000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
		{
			name: "file URI with no params adds all defaults",
			in:   "file:/var/lib/woodpecker/woodpecker.sqlite",
			wantKV: map[string]string{
				"_busy_timeout": "30000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
		{
			name: "user-supplied busy_timeout wins",
			in:   "/db.sqlite?_busy_timeout=60000",
			wantKV: map[string]string{
				"_busy_timeout": "60000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
		{
			name: "user-supplied journal_mode wins",
			in:   "/db.sqlite?_journal_mode=DELETE",
			wantKV: map[string]string{
				"_journal_mode": "DELETE",
				"_busy_timeout": "30000",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
		{
			name: "all user-supplied preserved",
			in:   "/db.sqlite?_busy_timeout=1000&_journal_mode=MEMORY&_synchronous=OFF&_txlock=deferred",
			wantKV: map[string]string{
				"_busy_timeout": "1000",
				"_journal_mode": "MEMORY",
				"_synchronous":  "OFF",
				"_txlock":       "deferred",
			},
		},
		{
			name: "unrelated params preserved alongside defaults",
			in:   "/db.sqlite?cache=shared&mode=rwc",
			wantKV: map[string]string{
				"cache":         "shared",
				"mode":          "rwc",
				"_busy_timeout": "30000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
		{
			name: "in-memory sqlite stays in-memory",
			in:   ":memory:",
			wantKV: map[string]string{
				"_busy_timeout": "30000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := applySqliteDefaults(tc.in)
			base, query := splitDSN(got)
			if base == "" {
				t.Fatalf("applySqliteDefaults(%q) returned empty base; got %q", tc.in, got)
			}
			values, err := url.ParseQuery(query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", query, err)
			}
			for k, want := range tc.wantKV {
				if got := values.Get(k); got != want {
					t.Errorf("%s: key %q = %q, want %q (full DSN: %q)", tc.name, k, got, want, got)
				}
			}
		})
	}
}

func TestApplySqliteDefaults_IdempotentOnRepeatedCall(t *testing.T) {
	once := applySqliteDefaults("/db.sqlite")
	twice := applySqliteDefaults(once)
	if once != twice {
		t.Errorf("not idempotent:\n  once:  %q\n  twice: %q", once, twice)
	}
}

func splitDSN(dsn string) (base, query string) {
	if i := strings.Index(dsn, "?"); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}
