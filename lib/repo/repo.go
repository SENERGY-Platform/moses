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
	"strings"

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

	// Approaching holds the zone values that are moving towards a set point,
	// keyed by zone id and state key. It is stored, so a restart continues an
	// approach instead of jumping to its target.
	Approaching map[string]map[string]Approach `json:"approaching,omitempty" bson:"approaching,omitempty"`

	// Set by the store, not by callers.
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

// Approach is one value on its way to a set point, following
// target + (from-target) * exp(-elapsed/tau). Both ends and the start time are
// stored rather than only the current value, so the curve is exact for any step
// and does not depend on how often it is read.
type Approach struct {
	From       float64 `json:"from" bson:"from"`
	Target     float64 `json:"target" bson:"target"`
	StartUnix  int64   `json:"start_unix" bson:"start_unix"`
	TauSeconds int64   `json:"tau_seconds" bson:"tau_seconds"`
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
