// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package main

import (
	"testing"
)

func TestSqliteFilePathFromDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "bare path",
			dsn:  "/var/lib/woodpecker/woodpecker.sqlite",
			want: "/var/lib/woodpecker/woodpecker.sqlite",
		},
		{
			name: "file scheme prefix",
			dsn:  "file:/var/lib/woodpecker/woodpecker.sqlite",
			want: "/var/lib/woodpecker/woodpecker.sqlite",
		},
		{
			name: "query string",
			dsn:  "/var/lib/woodpecker/woodpecker.sqlite?_busy_timeout=5000",
			want: "/var/lib/woodpecker/woodpecker.sqlite",
		},
		{
			name: "file scheme + query string (the fork#41 case)",
			dsn:  "file:/var/lib/woodpecker/woodpecker.sqlite?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on&cache=shared",
			want: "/var/lib/woodpecker/woodpecker.sqlite",
		},
		{
			name: "relative path",
			dsn:  "file:woodpecker.sqlite?_busy_timeout=5000",
			want: "woodpecker.sqlite",
		},
		{
			name: "empty",
			dsn:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sqliteFilePathFromDSN(tc.dsn)
			if got != tc.want {
				t.Errorf("sqliteFilePathFromDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
		})
	}
}
