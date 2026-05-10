// Copyright 2026 Woodpecker Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0

package grpc

import (
	"testing"

	"github.com/stretchr/testify/assert"

	queue_mocks "go.woodpecker-ci.org/woodpecker/v3/server/queue/mocks"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
)

// TestNewRPCForTesting confirms the test helper builds a usable *RPC
// without registering Prometheus collectors. Calling it twice in the
// same process must NOT panic — that's the whole point of having it
// separate from NewRPC.
func TestNewRPCForTesting(t *testing.T) {
	q := queue_mocks.NewMockQueue(t)
	s := store_mocks.NewMockStore(t)

	r1 := NewRPCForTesting(q, s)
	assert.NotNil(t, r1)
	assert.Equal(t, q, r1.queue)
	assert.Equal(t, s, r1.store)

	// The whole reason this constructor exists: re-creating RPC must
	// not panic on duplicate Prometheus collector registration.
	r2 := NewRPCForTesting(q, s)
	assert.NotNil(t, r2)
}
