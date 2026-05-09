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
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// TaskList returns persisted tasks ordered by dispatch priority — highest
// priority first, FIFO within ties via created. Used by the persistent queue
// at startup to seed the in-memory queue (#45 foundation for #46/#47); a
// missing ORDER BY would let a server restart momentarily run lower-priority
// tasks before the dispatcher catches up.
func (s storage) TaskList() ([]*model.Task, error) {
	tasks := make([]*model.Task, 0, perPage)
	return tasks, s.engine.OrderBy("priority DESC, created ASC").Find(&tasks)
}

func (s storage) TaskInsert(task *model.Task) error {
	return s.wq.serialize(func() error {
		// only Insert set auto created ID back to object
		return wrapInsert(s.writeEngine().Insert(task))
	})
}

func (s storage) TaskDelete(id string) error {
	return s.wq.serialize(func() error {
		return wrapDelete(s.writeEngine().Where("id = ?", id).Delete(new(model.Task)))
	})
}
