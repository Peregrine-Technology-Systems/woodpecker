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

// sqliteSafeDefaults is the minimum set of pragmas that lets Woodpecker
// survive concurrent webhook bursts without SQLITE_BUSY "database is locked"
// errors on /api/hook. See woodpecker-server#10 for the incident analysis.
var sqliteSafeDefaults = map[string]string{
	"_busy_timeout": "30000",
	"_journal_mode": "WAL",
	"_synchronous":  "NORMAL",
	"_txlock":       "immediate",
}

// applySqliteDefaults merges sqliteSafeDefaults into the DSN, preserving any
// value the operator has already set. Invoked from NewEngine when the driver
// is sqlite3.
func applySqliteDefaults(dsn string) string {
	base, rawQuery := splitSqliteDSN(dsn)
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		values = url.Values{}
	}
	for k, v := range sqliteSafeDefaults {
		if values.Get(k) == "" {
			values.Set(k, v)
		}
	}
	return base + "?" + values.Encode()
}

func splitSqliteDSN(dsn string) (base, query string) {
	if i := strings.Index(dsn, "?"); i >= 0 {
		return dsn[:i], dsn[i+1:]
	}
	return dsn, ""
}
