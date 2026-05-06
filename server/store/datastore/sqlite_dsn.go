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

// applySQLiteReaderDefaults merges sqliteReaderDefaults into the DSN.
func applySQLiteReaderDefaults(dsn string) string {
	return applySQLiteDefaults(dsn, sqliteReaderDefaults)
}

// applySQLiteWriterDefaults merges sqliteWriterDefaults into the DSN.
func applySQLiteWriterDefaults(dsn string) string {
	return applySQLiteDefaults(dsn, sqliteWriterDefaults)
}

func applySQLiteDefaults(dsn string, defaults map[string]string) string {
	base, rawQuery := splitSqliteDSN(dsn)
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		values = url.Values{}
	}
	for k, v := range defaults {
		if values.Get(k) == "" {
			values.Set(k, v)
		}
	}
	return base + "?" + values.Encode()
}

// applySqliteDefaults is kept for backward compatibility with existing tests.
// New code should call applySQLiteReaderDefaults or applySQLiteWriterDefaults.
func applySqliteDefaults(dsn string) string {
	return applySQLiteWriterDefaults(dsn)
}

func splitSqliteDSN(dsn string) (base, query string) {
	if i := strings.Index(dsn, "?"); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}
