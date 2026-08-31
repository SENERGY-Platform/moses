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
	"fmt"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
)

var ErrNotFound = errors.New("environment not found")

// ErrVersionConflict is what a write gets when the stored document has moved on
// since the caller read it. Every conflict is reported as a
// *VersionConflictError wrapping this, so a caller can either match the class
// with errors.Is or read both versions out with errors.As.
var ErrVersionConflict = errors.New("version conflict")

// VersionConflictError carries both sides of a refused compare-and-swap. Both
// numbers are in the message because the only useful reaction is to read the
// document again, and a caller that cannot see how far behind it was cannot
// tell a stale editor from a lost write.
type VersionConflictError struct {
	Id string
	// Expected is the version the caller carried.
	Expected int64
	// Stored is the version the document carries now, meaningful unless Gone or
	// StoredUnknown say otherwise.
	Stored int64
	// Gone says the document is not there at all - a different message and a
	// different fix. Not derived from Stored being zero: a pre-version-field
	// document is stored and reads as zero too, and reporting it as gone would
	// send its author looking for the wrong thing.
	Gone bool
	// StoredUnknown says the stored version could not be read back - a refused
	// write and a failed read at once. Naming a version that was never read
	// would be worse than admitting it: the advice is the same either way.
	StoredUnknown bool
}

func (this *VersionConflictError) Error() string {
	switch {
	case this.Gone:
		return fmt.Sprintf("the document was changed since you read it: you carry version %d, and it no longer exists — reload and redo your change", this.Expected)
	case this.StoredUnknown:
		return fmt.Sprintf("the document was changed since you read it: you carry version %d, and the stored one could not be read — reload and redo your change", this.Expected)
	}
	return fmt.Sprintf("the document was changed since you read it: you carry version %d, stored is %d — reload and redo your change", this.Expected, this.Stored)
}

// Unwrap makes errors.Is(err, ErrVersionConflict) work, so a caller that only
// wants the status code does not have to know the type.
func (this *VersionConflictError) Unwrap() error { return ErrVersionConflict }

// RuntimeState holds the live values of one environment: initial_states as
// evolved by the running simulation.
type RuntimeState struct {
	EnvironmentId string `json:"environment_id" bson:"environment_id"`

	// Keyed by id rather than mirroring the zone tree, so one zone can be
	// written without touching the rest.
	Context map[string]interface{}            `json:"context" bson:"context"`
	Zones   map[string]map[string]interface{} `json:"zones" bson:"zones"`
	Assets  map[string]map[string]interface{} `json:"assets" bson:"assets"`

	// Anchors holds the replay start per dataset channel, so a restart resumes
	// a looping replay mid-loop instead of starting the data over.
	Anchors map[string]int64 `json:"anchors,omitempty" bson:"anchors,omitempty"`

	// LastPublished holds, per channel with a change trigger, the value that
	// last went out and when - what the next computed value is compared
	// against, so it has to survive a restart or every such channel would
	// publish once and restart its heartbeat gap on every deployment.
	//
	// A sibling of Anchors rather than an asset state entry: it is runtime
	// bookkeeping no client can reach, and keeping it out of the asset map
	// avoids colliding with the meter reading a cumulative profile stores there
	// under the same channel id.
	LastPublished map[string]PublishedValue `json:"last_published,omitempty" bson:"last_published,omitempty"`

	// ScheduleRuns holds, per channel with a schedule source, where its
	// programme currently stands, so a restart does not put every declared
	// machine back at its first state - a gated one would otherwise sit closed
	// until the next rising edge, which for a shift calendar is the next
	// morning.
	//
	// A sibling of Anchors and LastPublished, for the same reasons: runtime
	// bookkeeping no client can reach, kept out of the asset map so it cannot
	// collide with the state keys the schedule itself writes there.
	ScheduleRuns map[string]ScheduleRun `json:"schedule_runs,omitempty" bson:"schedule_runs,omitempty"`

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

// PublishedValue is one reading that reached the platform: the number and the
// second it was sent in. The time is stored next to the value, not derived from
// UpdatedAtUnix, because the heartbeat gap of a channel publishing on change is
// measured from its own last publish - a state written for another channel says
// nothing about this one.
type PublishedValue struct {
	Value  float64 `json:"value" bson:"value"`
	AtUnix int64   `json:"at_unix" bson:"at_unix"`
}

// ScheduleRun is where one schedule channel stands: the instant its current
// pass through the states began, how many whole cycles were already behind
// that instant, and whether its gate is open.
//
// StartUnix and CycleOffset are one anchor in two parts: a cycling schedule
// rolls StartUnix forward by consumed cycles so the walk to now stays short,
// while CycleOffset keeps counting them so the duration draw stays on the
// absolute cycle count and the roll-forward stays invisible in the values.
//
// Open is the edge detector of a gated schedule - the cycle restarts at the
// first state on every rise - so it has to survive a restart, or a service
// that came back mid-shift would start the programme over.
type ScheduleRun struct {
	StartUnix   int64 `json:"start_unix" bson:"start_unix"`
	CycleOffset int64 `json:"cycle_offset" bson:"cycle_offset"`

	// PassUnix is the salt of the pass that is running: set to StartUnix when
	// the run is created and on every rising edge, then never moved again, while
	// StartUnix wanders with the roll-forward. A gated schedule draws its
	// durations on this number, so two mornings differ and moving the anchor
	// cannot redraw the shift that is running.
	PassUnix int64 `json:"pass_unix,omitempty" bson:"pass_unix,omitempty"`

	Open bool `json:"open" bson:"open"`
}

// Environments stores environment definitions.
type Environments interface {
	// Put replaces any previous definition under the same id without a
	// concurrency check, and returns the version the stored document carries
	// afterwards. The version is counted by the store itself, never taken from
	// the document handed in, so two writers arriving at once cannot end up on
	// the same number.
	//
	// A document that did not exist yet starts at version 1.
	Put(ctx context.Context, env domain.Environment) (version int64, err error)

	// PutIfVersion is Put with a compare-and-swap: it writes only while the
	// stored document still carries expectedVersion, and reports a
	// *VersionConflictError otherwise. The comparison and the write are one
	// operation in the store - checking first and writing after would leave the
	// same race, only narrower.
	//
	// expectedVersion must be 1 or higher. A caller that does not know the
	// stored version calls Put; passing zero here is refused rather than
	// silently downgraded to an unchecked write.
	PutIfVersion(ctx context.Context, env domain.Environment, expectedVersion int64) (version int64, err error)

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
