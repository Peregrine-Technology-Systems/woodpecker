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

// addPipelineKillReason adds kill_reason + killed_at columns to the
// pipelines table (#202). Stamped at every Killed/Canceled/Superseded
// transition so ops can attribute kills without a stack-trace dive.
//
// Backwards compatible: empty kill_reason on rows that existed before
// the migration is fine and treated as "unknown" by consumers.
//
// xorm Sync detects the new fields on model.Pipeline and ALTER TABLE
// ADD COLUMNs them in place. No data backfill (existing rows have
// kill_reason="" / killed_at=0, both already the zero value).
//
// No rollback (consistent with the 28 prior migrations).
var addPipelineKillReason = xormigrate.Migration{
	ID: "add-pipeline-kill-reason",
	MigrateSession: func(sess *xorm.Session) error {
		type pipeline struct {
			ID         int64  `xorm:"pk autoincr 'id'"`
			KillReason string `xorm:"varchar(64) 'kill_reason'"`
			KilledAt   int64  `xorm:"INDEX 'killed_at'"`
		}
		return sess.Sync(new(pipeline))
	},
}
