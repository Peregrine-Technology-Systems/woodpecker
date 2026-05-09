// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package datastore

import (
	"xorm.io/builder"

	"go.woodpecker-ci.org/woodpecker/v3/server/model"
)

// defaultPriorityAuditLimit is used when the caller passes Limit <= 0.
const defaultPriorityAuditLimit = 50

// maxPriorityAuditLimit caps a single response so a misconfigured client
// can't OOM the server requesting the entire audit log in one shot.
const maxPriorityAuditLimit = 200

// PriorityAuditInsert writes one audit row per priority mutation. ts is
// auto-filled by the xorm `created` magic tag, so callers don't need to
// stamp it. (#48)
func (s storage) PriorityAuditInsert(audit *model.PriorityAudit) error {
	return s.wq.serialize(func() error {
		return wrapInsert(s.writeEngine().Insert(audit))
	})
}

// PriorityAuditList reads the audit log in id-descending order with cursor
// pagination by id. Using id rather than ts as the cursor avoids boundary
// duplicates when many rows share a one-second timestamp. (#48)
func (s storage) PriorityAuditList(opts *model.PriorityAuditListOptions) ([]*model.PriorityAudit, error) {
	limit := defaultPriorityAuditLimit
	if opts != nil && opts.Limit > 0 {
		limit = opts.Limit
	}
	if limit > maxPriorityAuditLimit {
		limit = maxPriorityAuditLimit
	}

	sess := s.engine.Desc("id").Limit(limit)
	if opts != nil && opts.BeforeID > 0 {
		sess = sess.Where(builder.Lt{"id": opts.BeforeID})
	}

	rows := make([]*model.PriorityAudit, 0, limit)
	return rows, sess.Find(&rows)
}
