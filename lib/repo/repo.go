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

// Package repo stores environment definitions and their runtime state.
//
// The two are deliberately separate collections. A definition changes when a
// user edits it; runtime state changes on every tick of every channel. Keeping
// them in one document is what makes the current implementation rewrite an
// entire world on each state.set().
package repo

import (
	"context"
	"errors"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

// ErrNotFound is returned when no environment with the requested id exists.
var ErrNotFound = errors.New("environment not found")

// RuntimeState holds the live values of one environment: what the definition
// calls initial_states, evolved by the running simulation.
type RuntimeState struct {
	EnvironmentId string `json:"environment_id" bson:"environment_id"`

	// Context, Zones and Assets are keyed by id. Keeping them flat rather than
	// mirroring the zone tree means a single zone's values can be written
	// without touching the rest.
	Context map[string]interface{}            `json:"context" bson:"context"`
	Zones   map[string]map[string]interface{} `json:"zones" bson:"zones"`
	Assets  map[string]map[string]interface{} `json:"assets" bson:"assets"`

	// UpdatedAtUnix is set by the store on every write.
	UpdatedAtUnix int64 `json:"updated_at_unix" bson:"updated_at_unix"`
}

// StateChange is a partial RuntimeState: what a caller wants to set, and
// nothing about the values it does not mention. It is the shape of the knob an
// operator turns - outdoor temperature, a machine's speed - and it is applied to
// the running environment, not to the store, because the runtime holds the live
// state and its next flush would overwrite a direct write.
type StateChange struct {
	Context map[string]interface{}            `json:"context"`
	Zones   map[string]map[string]interface{} `json:"zones"`
	Assets  map[string]map[string]interface{} `json:"assets"`
}

// Empty reports whether the change would do nothing.
func (this StateChange) Empty() bool {
	return len(this.Context) == 0 && len(this.Zones) == 0 && len(this.Assets) == 0
}

// ErrNotRunning is returned when a state change addresses an environment the
// runtime does not hold. Stored but not running is the normal case for an
// environment whose definition was just written on another instance.
var ErrNotRunning = errors.New("the environment is not running")

// UnknownIdsError names the zone and asset ids a change referred to that the
// definition does not have. Reported rather than ignored: a key written under an
// id nothing reads is state that looks set and has no effect.
type UnknownIdsError struct {
	Zones  []string
	Assets []string
}

func (this *UnknownIdsError) Error() string {
	parts := []string{}
	if len(this.Zones) > 0 {
		parts = append(parts, "unknown zones: "+strings.Join(this.Zones, ", "))
	}
	if len(this.Assets) > 0 {
		parts = append(parts, "unknown assets: "+strings.Join(this.Assets, ", "))
	}
	return strings.Join(parts, "; ")
}

// Environments stores environment definitions.
type Environments interface {
	// Put stores env under its id, replacing any previous definition. It is
	// idempotent: storing an unchanged document twice changes nothing.
	Put(ctx context.Context, env domain.Environment) error

	// Get returns the definition, or ErrNotFound.
	Get(ctx context.Context, id string) (domain.Environment, error)

	// ListByOwner returns every environment owned by owner, ordered by name.
	ListByOwner(ctx context.Context, owner string) ([]domain.Environment, error)

	// All returns every environment, for loading the simulation on startup.
	// A definition that cannot be decoded is skipped and logged rather than
	// failing the whole load.
	All(ctx context.Context) ([]domain.Environment, error)

	// Delete removes the definition and its runtime state. Deleting something
	// that does not exist is not an error.
	Delete(ctx context.Context, id string) error
}

// States stores the live values of running environments.
type States interface {
	// Load returns the stored state, or an empty state if none exists yet.
	Load(ctx context.Context, environmentId string) (RuntimeState, error)

	// Save writes the whole state of one environment. Callers are expected to
	// throttle: this is the write that happens often.
	Save(ctx context.Context, state RuntimeState) error

	// Delete removes the state of one environment.
	Delete(ctx context.Context, environmentId string) error
}
