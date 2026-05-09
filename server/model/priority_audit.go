// Copyright 2026 Peregrine Technology Systems
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package model

// PriorityAudit captures a single mutation to a task's queue priority.
// One row is written per successful PATCH against the priority API (#47).
// Required for SOC 2 CC7.2 / ISO 27001 A.12.4 traceability — without an
// append-only record of who reordered what and when, the audit-trail-as-
// deterrent design from peregrine-grafana#184 doesn't hold up. (#48)
type PriorityAudit struct {
	ID          int64  `json:"id"           xorm:"pk autoincr 'id'"`
	ActorEmail  string `json:"actor_email"  xorm:"NOT NULL 'actor_email'"`
	TaskID      string `json:"task_id"      xorm:"NOT NULL 'task_id'"`
	OldPriority int64  `json:"old_priority" xorm:"NOT NULL DEFAULT 0 'old_priority'"`
	NewPriority int64  `json:"new_priority" xorm:"NOT NULL DEFAULT 0 'new_priority'"`
	TS          int64  `json:"ts"           xorm:"'ts' NOT NULL DEFAULT 0 INDEX created"`
} //	@name	PriorityAudit

// TableName returns the database table name for xorm.
func (PriorityAudit) TableName() string {
	return "priority_audit"
}

// PriorityAuditListOptions controls cursor-paginated reads from the audit
// log. Cursor is the row id (autoincrement PK) — using id rather than ts
// avoids boundary duplicates when many rows share a one-second timestamp.
type PriorityAuditListOptions struct {
	// Limit caps the number of rows returned. Implementations clamp to a
	// safe maximum (defaultPriorityAuditMaxLimit) to keep response size
	// bounded.
	Limit int
	// BeforeID returns rows with id strictly less than this value. Pass 0
	// to retrieve the most recent page.
	BeforeID int64
}
