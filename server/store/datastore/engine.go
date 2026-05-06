// Copyright 2021 Woodpecker Authors
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

package datastore

import (
	"context"

	"github.com/rs/zerolog"
	"xorm.io/xorm"
	xlog "xorm.io/xorm/log"

	"go.woodpecker-ci.org/woodpecker/v3/server/store"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/datastore/migration"
)

type storage struct {
	engine *xorm.Engine // reader pool (MaxOpenConns=10 for SQLite, opts value for others)
	// writer is a dedicated single-connection engine used exclusively by the
	// write queue drain goroutine for SQLite. Separating read and write
	// connections lets WAL serve concurrent reads while guaranteeing a single
	// writer at the SQLite level (#107). nil for non-SQLite drivers.
	writer *xorm.Engine
	// wq serializes writes for SQLite to prevent SQLITE_BUSY under concurrent
	// load (#55). nil for non-SQLite drivers (serialize() calls fn directly).
	wq *writeQueue
}

const perPage = 50

// writeEngine returns the dedicated writer connection for SQLite, or the
// shared engine for other drivers. Use this inside s.wq.serialize closures.
func (s storage) writeEngine() *xorm.Engine {
	if s.writer != nil {
		return s.writer
	}
	return s.engine
}

func NewEngine(opts *store.Opts) (store.Store, error) {
	dsn := opts.Config
	// Reader DSN: WAL + busy_timeout, but NO _txlock=immediate.
	// Readers use deferred locking so they do not compete for the RESERVED lock
	// that the writer holds. Applying _txlock=immediate to readers caused all
	// reader connections to fight for the same lock under concurrent webhook
	// load, producing "database table is locked" on /api/hook (#114).
	readerDSN := dsn
	writerDSN := dsn
	if opts.Driver == "sqlite3" {
		readerDSN = applySQLiteReaderDefaults(dsn)
		writerDSN = applySQLiteWriterDefaults(dsn)
	}
	engine, err := xorm.NewEngine(opts.Driver, readerDSN)
	if err != nil {
		return nil, err
	}

	level := xlog.LogLevel(zerolog.GlobalLevel())
	if !opts.XORM.Log {
		level = xlog.LOG_OFF
	}

	logger := newXORMLogger(level)
	engine.SetLogger(logger)
	engine.ShowSQL(opts.XORM.ShowSQL)
	engine.SetMaxOpenConns(opts.XORM.MaxOpenConns)
	engine.SetMaxIdleConns(opts.XORM.MaxIdleConns)
	engine.SetConnMaxLifetime(opts.XORM.ConnMaxLifetime)

	s := &storage{engine: engine}
	if opts.Driver == "sqlite3" {
		// Reader pool: allow concurrent reads via WAL. Capped at 10 — d3ci42
		// is a small machine and hitting 10 simultaneous readers is unlikely.
		engine.SetMaxOpenConns(10)
		engine.SetMaxIdleConns(5)

		// Dedicated writer: a separate single-connection engine used only by the
		// write queue drain goroutine. WAL allows readers and this one writer to
		// coexist without blocking. MaxOpenConns=1 here guarantees no goroutine
		// other than the drain goroutine ever opens a write connection (#107).
		// Uses _txlock=immediate to prevent upgrade-deadlocks on the write path.
		writer, err := xorm.NewEngine(opts.Driver, writerDSN)
		if err != nil {
			return nil, err
		}
		writer.SetLogger(logger)
		writer.ShowSQL(opts.XORM.ShowSQL)
		writer.SetMaxOpenConns(1)
		writer.SetMaxIdleConns(1)
		s.writer = writer

		s.wq = newWriteQueue(writeQueueDepth)
	}
	return s, nil
}

func (s storage) Ping() error {
	return s.engine.Ping()
}

// Migrate old storage or init new one.
func (s storage) Migrate(ctx context.Context, allowLong bool) error {
	return migration.Migrate(ctx, s.engine, allowLong)
}

func (s storage) Close() error {
	if s.wq != nil {
		s.wq.close()
	}
	return s.engine.Close()
}
