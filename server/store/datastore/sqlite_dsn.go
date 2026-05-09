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

	"github.com/rs/zerolog/log"
)

// sqliteReaderDefaults are pragmas applied to the reader connection pool.
// _txlock is intentionally absent — reader transactions use SQLite's default
// deferred locking so they do NOT acquire a RESERVED lock at BEGIN. Applying
// _txlock=immediate to readers causes all reader connections to compete for the
// same RESERVED lock, blocking the writer and each other under concurrent load
// (#114).
var sqliteReaderDefaults = map[string]string{
	"_busy_timeout": "30000",
	"_journal_mode": "WAL",
	"_synchronous":  "NORMAL",
}

// sqliteWriterDefaults are pragmas applied to the single dedicated writer
// connection. _txlock=immediate acquires a RESERVED lock at BEGIN, preventing
// the upgrade-deadlock that can occur when a deferred transaction reads then
// tries to write while another writer is active.
var sqliteWriterDefaults = map[string]string{
	"_busy_timeout": "30000",
	"_journal_mode": "WAL",
	"_synchronous":  "NORMAL",
	"_txlock":       "immediate",
}

// sqliteForbiddenParams must never appear on either reader or writer DSNs.
// cache=shared switches SQLite to in-process shared-cache mode, where readers
// and writers contend at the table level; under load this surfaces as
// "database table is locked: <table>" errors that bypass busy_timeout retry
// (#177). The Go binary is single-process and gains nothing from shared cache.
var sqliteForbiddenParams = []string{"cache"}

// applySQLiteReaderDefaults builds the reader DSN. _txlock is force-stripped —
// operator override is rejected because RESERVED-lock contention from #114
// recurs immediately if any value (immediate or otherwise) is set on readers.
func applySQLiteReaderDefaults(dsn string) string {
	base, values := parseSqliteDSN(dsn)
	stripParams(values, "reader", sqliteForbiddenParams...)
	stripParams(values, "reader", "_txlock")
	fillDefaults(values, sqliteReaderDefaults)
	return base + "?" + values.Encode()
}

// applySQLiteWriterDefaults builds the writer DSN. _txlock is force-set to
// immediate — operator override is rejected because deferred locking can
// produce upgrade-deadlocks under concurrent write load.
func applySQLiteWriterDefaults(dsn string) string {
	base, values := parseSqliteDSN(dsn)
	stripParams(values, "writer", sqliteForbiddenParams...)
	forceParam(values, "writer", "_txlock", "immediate")
	fillDefaults(values, sqliteWriterDefaults)
	return base + "?" + values.Encode()
}

// applySqliteDefaults is kept for backward compatibility with existing tests.
// New code should call applySQLiteReaderDefaults or applySQLiteWriterDefaults.
func applySqliteDefaults(dsn string) string {
	return applySQLiteWriterDefaults(dsn)
}

func parseSqliteDSN(dsn string) (string, url.Values) {
	base, rawQuery := splitSqliteDSN(dsn)
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		values = url.Values{}
	}
	return base, values
}

func splitSqliteDSN(dsn string) (base, query string) {
	if i := strings.Index(dsn, "?"); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}

func stripParams(values url.Values, role string, keys ...string) {
	for _, k := range keys {
		if existing := values.Get(k); existing != "" {
			log.Warn().
				Str("dsn_role", role).
				Str("param", k).
				Str("operator_value", existing).
				Msg("sqlite DSN: stripping forbidden param to prevent lock contention (#177)")
			values.Del(k)
		}
	}
}

func forceParam(values url.Values, role, key, want string) {
	if existing := values.Get(key); existing != "" && existing != want {
		log.Warn().
			Str("dsn_role", role).
			Str("param", key).
			Str("operator_value", existing).
			Str("forced_value", want).
			Msg("sqlite DSN: forcing param value (operator override rejected for correctness)")
	}
	values.Set(key, want)
}

func fillDefaults(values url.Values, defaults map[string]string) {
	for k, v := range defaults {
		if values.Get(k) == "" {
			values.Set(k, v)
		}
	}
}
