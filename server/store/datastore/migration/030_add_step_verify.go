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

// addStepVerify adds a verify JSON column to the steps table (#235). Holds the
// compiled outcome-verification proof-query so the server can read it at step
// kill time without the by-then-deleted queue task.
//
// Backwards compatible: rows from before the migration have verify=NULL,
// treated as "no verification" by consumers.
//
// xorm Sync detects the new field on model.Step and ALTER TABLE ADD COLUMNs it
// in place. No data backfill (existing rows have verify=NULL, the zero value).
//
// No rollback (consistent with the prior migrations).
var addStepVerify = xormigrate.Migration{
	ID: "add-step-verify",
	MigrateSession: func(sess *xorm.Session) error {
		type step struct {
			ID     int64  `xorm:"pk autoincr 'id'"`
			Verify string `xorm:"json 'verify'"`
		}
		return sess.Sync(new(step))
	},
}
