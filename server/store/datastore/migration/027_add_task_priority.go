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

// addTaskPriority adds priority + created to the tasks table and a composite
// index spanning both, foundation for the queue priority dispatcher (#46) and
// the admin reorder API (#47). Default priority=0 preserves FIFO semantics;
// existing rows keep created=0 and naturally drain first (in rowid order)
// before any post-migration insert, which gets created=time.Now().Unix() via
// xorm's `created` magic tag.
//
// No rollback: Woodpecker's xormigrate use across 27 prior migrations has
// never registered Rollback hooks. An operator downgrading to a pre-#45
// binary against a migrated DB sees the extra column harmlessly ignored.
var addTaskPriority = xormigrate.Migration{
	ID: "add-task-priority",
	MigrateSession: func(sess *xorm.Session) error {
		type tasks struct {
			ID       string `xorm:"PK UNIQUE 'id'"`
			Priority int64  `xorm:"'priority' NOT NULL DEFAULT 0 INDEX(priority_created)"`
			Created  int64  `xorm:"'created' NOT NULL DEFAULT 0 INDEX(priority_created) created"`
		}
		return sess.Sync(new(tasks))
	},
}
