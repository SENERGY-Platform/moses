/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package repo stores environment definitions and their runtime state in two
// collections: a definition changes when a user edits it, state on every tick.
package repo

import (
	"context"
	"errors"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

var ErrNotFound = errors.New("environment not found")

// RuntimeState holds the live values of one environment: initial_states as
// evolved by the running simulation.
type RuntimeState struct {
	EnvironmentId string `json:"environment_id" bson:"environment_id"`

	// Keyed by id rather than mirroring the zone tree, so one zone can be
	// written without touching the rest.
	Context map[string]interface{}            `json:"context" bson:"context"`
	Zones   map[string]map[string]interface{} `json:"zones" bson:"zones"`
	Assets  map[string]map[string]interface{} `json:"assets" bson:"assets"`

	// Set by the store, not by callers.
	UpdatedAtUnix int64 `json:"updated_at_unix" bson:"updated_at_unix"`
}

// Environments stores environment definitions.
type Environments interface {
	// Put replaces any previous definition under the same id.
	Put(ctx context.Context, env domain.Environment) error

	// Get returns ErrNotFound if no such environment exists.
	Get(ctx context.Context, id string) (domain.Environment, error)

	// ListByOwner is ordered by name.
	ListByOwner(ctx context.Context, owner string) ([]domain.Environment, error)

	// All skips and logs an undecodable definition instead of failing the load.
	All(ctx context.Context) ([]domain.Environment, error)

	// Delete also removes the runtime state, and tolerates a missing id.
	Delete(ctx context.Context, id string) error
}

// States stores the live values of running environments.
type States interface {
	// Load returns an empty state if none is stored yet.
	Load(ctx context.Context, environmentId string) (RuntimeState, error)

	// Save writes the whole state. Callers throttle; this is the frequent write.
	Save(ctx context.Context, state RuntimeState) error

	Delete(ctx context.Context, environmentId string) error
}
