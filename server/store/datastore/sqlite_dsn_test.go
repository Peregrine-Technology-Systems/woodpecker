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
		name   string
		in     string
		wantKV map[string]string
		// absentKeys: keys that must NOT appear in the resulting DSN
		absentKeys []string
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
			// #177: writer must force _txlock=immediate. Operator override
			// to "deferred" is rejected because deferred locking can
			// upgrade-deadlock under concurrent write load.
			name: "writer forces _txlock=immediate over operator deferred",
			in:   "/db.sqlite?_busy_timeout=1000&_journal_mode=MEMORY&_synchronous=OFF&_txlock=deferred",
			wantKV: map[string]string{
				"_busy_timeout": "1000",
				"_journal_mode": "MEMORY",
				"_synchronous":  "OFF",
				"_txlock":       "immediate",
			},
		},
		{
			// #177: cache=shared produces "database table is locked:
			// <table>" errors that bypass busy_timeout retry. Strip it
			// from both reader and writer DSNs regardless of operator
			// intent — the Go binary is single-process.
			name: "cache=shared stripped on writer",
			in:   "/db.sqlite?cache=shared&mode=rwc",
			wantKV: map[string]string{
				"mode":          "rwc",
				"_busy_timeout": "30000",
				"_journal_mode": "WAL",
				"_synchronous":  "NORMAL",
				"_txlock":       "immediate",
			},
			absentKeys: []string{"cache"},
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
					t.Errorf("%s: key %q = %q, want %q", tc.name, k, got, want)
				}
			}
			for _, k := range tc.absentKeys {
				if got := values.Get(k); got != "" {
					t.Errorf("%s: key %q must be absent, got %q", tc.name, k, got)
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

// TestApplySQLiteReaderDefaults_StripsTxlock locks in the #114 fix at the DSN
// boundary: regardless of operator intent, reader connections must never
// acquire a RESERVED lock at BEGIN. Any _txlock value (immediate, exclusive,
// deferred) is force-stripped from the reader DSN.
func TestApplySQLiteReaderDefaults_StripsTxlock(t *testing.T) {
	cases := []string{
		"/db.sqlite?_txlock=immediate",
		"/db.sqlite?_txlock=exclusive",
		"/db.sqlite?_txlock=deferred",
		"file:/db.sqlite?_busy_timeout=30000&_txlock=immediate",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got := applySQLiteReaderDefaults(in)
			_, query := splitDSN(got)
			values, err := url.ParseQuery(query)
			if err != nil {
				t.Fatalf("ParseQuery(%q): %v", query, err)
			}
			if v := values.Get("_txlock"); v != "" {
				t.Errorf("reader DSN must not contain _txlock; got %q (full DSN: %q)", v, got)
			}
		})
	}
}

// TestApplySQLiteReaderDefaults_StripsCacheShared locks in the #177 fix:
// cache=shared on the reader produces "database table is locked: <table>"
// (verified in mattn/go-sqlite3 sqlite3-binding.c:103525, inside
// SHARED_CACHE-only OP_TableLock handler). Strip regardless of operator
// intent — the Go binary is single-process.
func TestApplySQLiteReaderDefaults_StripsCacheShared(t *testing.T) {
	got := applySQLiteReaderDefaults("/db.sqlite?cache=shared&mode=rwc")
	_, query := splitDSN(got)
	values, err := url.ParseQuery(query)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", query, err)
	}
	if v := values.Get("cache"); v != "" {
		t.Errorf("reader DSN must not contain cache=shared; got %q (full DSN: %q)", v, got)
	}
	if v := values.Get("mode"); v != "rwc" {
		t.Errorf("reader DSN should preserve mode=rwc; got %q", v)
	}
}

// TestApplySQLiteReaderDefaults_FillsSafeDefaults verifies the reader still
// gets WAL + busy_timeout + synchronous=NORMAL when the operator hasn't set
// them.
func TestApplySQLiteReaderDefaults_FillsSafeDefaults(t *testing.T) {
	got := applySQLiteReaderDefaults("/db.sqlite")
	_, query := splitDSN(got)
	values, _ := url.ParseQuery(query)
	for k, want := range map[string]string{
		"_busy_timeout": "30000",
		"_journal_mode": "WAL",
		"_synchronous":  "NORMAL",
	} {
		if v := values.Get(k); v != want {
			t.Errorf("reader default %q = %q, want %q", k, v, want)
		}
	}
}

// TestApplySQLiteWriterDefaults_ForcesTxlockImmediate ensures the writer can
// never run with deferred locking, which would allow upgrade-deadlocks under
// concurrent write load.
func TestApplySQLiteWriterDefaults_ForcesTxlockImmediate(t *testing.T) {
	for _, in := range []string{
		"/db.sqlite?_txlock=deferred",
		"/db.sqlite?_txlock=exclusive",
		"/db.sqlite",
	} {
		t.Run(in, func(t *testing.T) {
			got := applySQLiteWriterDefaults(in)
			_, query := splitDSN(got)
			values, _ := url.ParseQuery(query)
			if v := values.Get("_txlock"); v != "immediate" {
				t.Errorf("writer DSN must force _txlock=immediate; got %q (full DSN: %q)", v, got)
			}
		})
	}
}

// TestApplySQLiteWriterDefaults_StripsCacheShared locks in the #177 fix on
// the writer side too — cache=shared on either engine triggers shared-cache
// table locking.
func TestApplySQLiteWriterDefaults_StripsCacheShared(t *testing.T) {
	got := applySQLiteWriterDefaults("/db.sqlite?cache=shared")
	_, query := splitDSN(got)
	values, _ := url.ParseQuery(query)
	if v := values.Get("cache"); v != "" {
		t.Errorf("writer DSN must not contain cache=shared; got %q (full DSN: %q)", v, got)
	}
}

func splitDSN(dsn string) (base, query string) {
	if i := strings.Index(dsn, "?"); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}
