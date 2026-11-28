// Copyright 2026 Woodpecker Authors
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

package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"go.woodpecker-ci.org/woodpecker/v3/rpc/proto"
	"go.woodpecker-ci.org/woodpecker/v3/server/model"
	store_mocks "go.woodpecker-ci.org/woodpecker/v3/server/store/mocks"
	"go.woodpecker-ci.org/woodpecker/v3/server/store/types"
)

// #419: the agent auth path, which the constant-time compare changed.
const authMasterToken = "master-secret-0123456789"

func authServer(t *testing.T) (*WoodpeckerAuthServer, *store_mocks.MockStore) {
	t.Helper()
	s := store_mocks.NewMockStore(t)
	return NewWoodpeckerAuthServer(NewJWTManager("jwt-secret"), authMasterToken, s), s
}

func TestAuth_MasterTokenRegistersANewSystemAgent(t *testing.T) {
	srv, s := authServer(t)
	s.On("AgentCreate", mock.Anything).Run(func(a mock.Arguments) { a.Get(0).(*model.Agent).ID = 7 }).Return(nil)

	res, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: -1, AgentToken: authMasterToken})

	require.NoError(t, err)
	assert.Equal(t, int64(7), res.AgentId)
	assert.NotEmpty(t, res.AccessToken)
}

func TestAuth_MasterTokenReconnectsAKnownSystemAgent(t *testing.T) {
	srv, s := authServer(t)
	s.On("AgentFind", int64(7)).Return(&model.Agent{ID: 7, OwnerID: model.IDNotSet, OrgID: model.IDNotSet}, nil)

	res, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: 7, AgentToken: authMasterToken})

	require.NoError(t, err)
	assert.Equal(t, int64(7), res.AgentId)
}

func TestAuth_MasterTokenRejectsAnUnknownOrNonSystemAgent(t *testing.T) {
	srv, s := authServer(t)
	s.On("AgentFind", int64(8)).Return(nil, types.ErrRecordNotExist)
	s.On("AgentFind", int64(9)).Return(nil, errors.New("db down"))
	s.On("AgentFind", int64(10)).Return(&model.Agent{ID: 10, OwnerID: 3}, nil)

	for id, want := range map[int64]string{8: "AgentID not found", 9: "db down", 10: "not a system agent"} {
		_, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: id, AgentToken: authMasterToken})
		assert.ErrorContains(t, err, want)
	}
}

// A token that is not the master secret, whatever its length, is looked up
// as an individual agent token, never accepted as the master secret.
func TestAuth_NearMissesOfTheMasterTokenAreNotTheMasterToken(t *testing.T) {
	for name, presented := range map[string]string{
		"one byte off": authMasterToken[:len(authMasterToken)-1] + "X",
		"a prefix":     authMasterToken[:6],
		"longer":       authMasterToken + "0",
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			srv, s := authServer(t)
			s.On("AgentFindByToken", presented).Return(nil, types.ErrRecordNotExist)

			_, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: -1, AgentToken: presented})

			assert.ErrorContains(t, err, "individual agent not found by token")
		})
	}
}

func TestAuth_IndividualAgentToken(t *testing.T) {
	srv, s := authServer(t)
	s.On("AgentFindByToken", "agent-own-token").Return(&model.Agent{ID: 12, OwnerID: 3}, nil)

	res, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: 12, AgentToken: "agent-own-token"})

	require.NoError(t, err)
	assert.Equal(t, int64(12), res.AgentId)
}

func TestAuth_SurfacesStoreErrors(t *testing.T) {
	srv, s := authServer(t)
	s.On("AgentCreate", mock.Anything).Return(errors.New("insert failed"))
	s.On("AgentFindByToken", "other").Return(nil, errors.New("db down"))

	_, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: -1, AgentToken: authMasterToken})
	assert.ErrorContains(t, err, "insert failed")
	_, err = srv.Auth(context.Background(), &proto.AuthRequest{AgentId: 5, AgentToken: "other"})
	assert.ErrorContains(t, err, "db down")
}

func TestAuth_WithoutAMasterTokenOnlyIndividualTokensWork(t *testing.T) {
	s := store_mocks.NewMockStore(t)
	srv := NewWoodpeckerAuthServer(NewJWTManager("jwt-secret"), "", s)
	s.On("AgentFindByToken", "").Return(nil, types.ErrRecordNotExist)

	_, err := srv.Auth(context.Background(), &proto.AuthRequest{AgentId: -1, AgentToken: ""})

	assert.ErrorContains(t, err, "individual agent not found by token")
}
