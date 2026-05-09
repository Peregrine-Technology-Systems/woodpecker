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

package migration

import (
	"src.techknowlogick.com/xormigrate"
	"xorm.io/xorm"
)

// addPriorityAudit creates the priority_audit table — append-only record of
// queue priority mutations for SOC 2 / ISO 27001 traceability (#48). The
// PATCH handler in #47 writes one row per successful priority change; the
// GET endpoint reads it back via cursor pagination by id. ts uses xorm's
// `created` magic so callers don't need to fill it.
//
// task_id is TEXT, not BIGINT (the issue's Postgres-flavored DDL was
// inaccurate): model.Task.ID is a string composite (workflow/step), not
// an integer.
//
// No FK constraint to tasks(id): Woodpecker's xorm models never declare
// FKs, and SQLite enforces FKs only with `PRAGMA foreign_keys=ON` (off by
// default). Application-level integrity is the contract here.
//
// No rollback (consistent with the 27 prior migrations).
var addPriorityAudit = xormigrate.Migration{
	ID: "add-priority-audit",
	MigrateSession: func(sess *xorm.Session) error {
		type priorityAudit struct {
			ID          int64  `xorm:"pk autoincr 'id'"`
			ActorEmail  string `xorm:"NOT NULL 'actor_email'"`
			TaskID      string `xorm:"NOT NULL 'task_id'"`
			OldPriority int64  `xorm:"NOT NULL DEFAULT 0 'old_priority'"`
			NewPriority int64  `xorm:"NOT NULL DEFAULT 0 'new_priority'"`
			TS          int64  `xorm:"'ts' NOT NULL DEFAULT 0 INDEX created"`
		}
		return sess.Sync(new(priorityAudit))
	},
}
