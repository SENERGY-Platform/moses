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

// Package migration plans the one-shot conversion of the legacy worlds into the
// environment model of lib/domain.
//
// Everything in here is pure: Plan() reads a map of legacy worlds and a set of
// already existing environment ids and returns what it intends to do. It does
// not read the database and it does not write anything, so the interesting part
// of a migration against production data can be unit tested, and the caller
// decides whether the plan is printed or executed.
package migration

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/state"
)

// ErrNilWorld is reported for a null entry in the world map. LoadWorlds() never
// produces one, but a nil pointer would panic in the conversion, and losing the
// rest of the migration to one broken entry is the wrong failure mode.
var ErrNilWorld = errors.New("the legacy world is null")

// ErrNoLegacyId is reported for a legacy world without an id.
//
// This blocks the world rather than migrating it: the environment id is the
// legacy world id, and FromLegacyWorld generates a fresh uuid when there is
// none. That uuid differs on every run, so a second run could not recognise the
// world as already migrated and would insert another copy of it - exactly the
// duplicate the skip detection exists to prevent.
var ErrNoLegacyId = errors.New("the legacy world has no id, so a repeated migration could not recognise it")

// ErrDuplicateEnvironmentId is reported when two legacy worlds of the same run
// convert into the same environment id. Writing both would mean the second
// silently replaces the first, because Put() is keyed by the id.
var ErrDuplicateEnvironmentId = errors.New("two legacy worlds convert into the same environment id")

// WorldPlan is what the migration intends to do with one legacy world.
//
// A plan is only written when Writable() is true, which means: the world is not
// already migrated and nothing blocks it. Problems are not blocking - they are
// the expected outcome of the conversion (ref domain.FromLegacyWorld) and the
// work list for re-modelling the change routines.
//
// On the skip: a world whose environment id already exists in the environment
// store is never overwritten. Re-running the migration has to be safe, and by
// the time it is re-run the stored environment may carry runtime state, manual
// corrections of the device kinds and zone types the conversion had to guess,
// and channel sources that replaced the change routines. All of that lives only
// in the new document; the legacy world does not know about it, so a second
// conversion would throw it away. Skipping is therefore not an optimisation but
// the only safe behaviour, and an intentional re-import has to delete the
// environment first.
type WorldPlan struct {
	// WorldId identifies the legacy world: its own id, or the key it was loaded
	// under when the document itself has none (which is a blocking condition,
	// ref ErrNoLegacyId, but still has to be reportable).
	WorldId string

	// WorldName is the legacy name, unsubstituted. Environment.Name may differ:
	// the conversion substitutes a placeholder for an empty name and reports
	// that as a Problem.
	WorldName string

	// Environment is the conversion result. It is filled even when the plan is
	// skipped or blocked, so that a dry run can report what would have been
	// written. It is the zero value only when the conversion never ran.
	Environment domain.Environment

	// Problems are the findings of the conversion, plus the findings of the
	// planning itself (a world without an owner). None of them blocks a write.
	Problems []domain.Problem

	// Err is why this plan must not be written, or nil. It carries the
	// *domain.ValidationError of domain.Validate in the normal case, and
	// ErrNilWorld, ErrNoLegacyId, ErrDuplicateEnvironmentId or the conversion's
	// own error otherwise.
	//
	// It is also set for a skipped world whose conversion would not validate.
	// That is information, not a failure: nothing is written either way, and the
	// stored environment - not this conversion - is what runs.
	Err error

	// Skip marks a world that already exists in the environment store.
	Skip bool

	// SkipReason is empty exactly when Skip is false.
	SkipReason string
}

// Writable reports whether this plan may be stored.
func (this WorldPlan) Writable() bool {
	return !this.Skip && this.Err == nil
}

// Blocked reports whether this plan wanted to be written but must not be. A
// skipped world is not blocked: nothing is wrong with it.
func (this WorldPlan) Blocked() bool {
	return !this.Skip && this.Err != nil
}

// Counts returns the size of the converted environment.
func (this WorldPlan) Counts() (zones int, assets int, channels int) {
	return countZones(this.Environment.Zones)
}

// routineMarker appears in the path of every problem that reports an unmapped
// change routine. FromLegacyWorld builds those paths as "...change_routines[key]".
const routineMarker = "change_routines["

// UnmappedRoutines returns the problems that report a change routine which was
// not migrated. They are the reason the legacy world has to be kept: their
// javascript is not in the converted document.
func (this WorldPlan) UnmappedRoutines() []domain.Problem {
	result := []domain.Problem{}
	for _, problem := range this.Problems {
		if strings.Contains(problem.Path, routineMarker) {
			result = append(result, problem)
		}
	}
	return result
}

// OtherProblems returns every problem that is not an unmapped change routine.
func (this WorldPlan) OtherProblems() []domain.Problem {
	result := []domain.Problem{}
	for _, problem := range this.Problems {
		if !strings.Contains(problem.Path, routineMarker) {
			result = append(result, problem)
		}
	}
	return result
}

// Plan decides what to do with every legacy world.
//
// existingIds is the set of environment ids that already exist in the
// environment store; the caller queries it. A world whose environment id is in
// that set is planned as a skip and never overwritten (ref WorldPlan).
//
// The result is ordered by world name and then by world id, so that two runs
// over the same data print the same report and a diff of two runs shows what
// changed rather than how the map happened to iterate.
func Plan(worlds map[string]*state.World, envType domain.EnvironmentType, existingIds map[string]bool) []WorldPlan {
	keys := make([]string, 0, len(worlds))
	for key := range worlds {
		keys = append(keys, key)
	}
	// sorted before converting, not after: the duplicate id detection below
	// depends on which world is seen first, so the input order has to be total
	// as well, not only the output order.
	sort.Slice(keys, func(a, b int) bool {
		nameA, nameB := worldName(worlds[keys[a]]), worldName(worlds[keys[b]])
		if nameA != nameB {
			return nameA < nameB
		}
		idA, idB := worldId(keys[a], worlds[keys[a]]), worldId(keys[b], worlds[keys[b]])
		if idA != idB {
			return idA < idB
		}
		return keys[a] < keys[b]
	})

	result := make([]WorldPlan, 0, len(keys))
	// produced maps an environment id to the world id that produced it, so that
	// a collision can name both sides.
	produced := map[string]string{}
	for _, key := range keys {
		result = append(result, planWorld(key, worlds[key], envType, existingIds, produced))
	}
	return result
}

func planWorld(key string, world *state.World, envType domain.EnvironmentType, existingIds map[string]bool, produced map[string]string) WorldPlan {
	result := WorldPlan{WorldId: strings.TrimSpace(key), Problems: []domain.Problem{}}
	if world == nil {
		result.Err = fmt.Errorf("%w (loaded under the key %q)", ErrNilWorld, key)
		return result
	}
	result.WorldId = worldId(key, world)
	result.WorldName = world.Name

	if strings.TrimSpace(world.Id) == "" {
		result.Err = fmt.Errorf("%w (loaded under the key %q)", ErrNoLegacyId, key)
		return result
	}

	// the world is passed by value, and FromLegacyWorld deep copies every state
	// map, so nothing of the legacy document is shared with the plan
	environment, problems, err := domain.FromLegacyWorld(*world, envType)
	if err != nil {
		// a caller mistake, currently only an unknown environment type. reported
		// per world rather than returned, because Plan has no error return and
		// the report has to name what was not migrated.
		result.Err = err
		return result
	}
	result.Environment = environment
	if problems != nil {
		result.Problems = problems
	}

	if strings.TrimSpace(environment.Owner) == "" {
		// an environment without an owner is not in anybody's list: ListByOwner
		// filters by exactly this field. The usual cause is not broken data but
		// the wrong input - state.World.Owner is bson only (json:"-"), so a world
		// read from a json export has no owner while the same world read from
		// mongo has.
		result.Problems = append(result.Problems, domain.Problem{
			Path:    "owner",
			Message: "the legacy world has no owner, so the environment would belong to nobody and would not appear in any user's list; note that a world read from a json export never carries an owner because the legacy field is bson only",
		})
	}

	if previous, taken := produced[environment.Id]; taken {
		result.Err = fmt.Errorf("%w: %v was already produced by the legacy world %v", ErrDuplicateEnvironmentId, environment.Id, previous)
		return result
	}
	produced[environment.Id] = result.WorldId

	if existingIds[environment.Id] {
		result.Skip = true
		result.SkipReason = fmt.Sprintf("an environment with the id %v already exists and is not overwritten; delete it first to import the legacy world again", environment.Id)
	}

	// validated for a skipped world as well: it costs nothing and a converted
	// document that would not validate is worth seeing even when it is not
	// written, because it means the legacy world and the stored environment have
	// drifted apart.
	result.Err = domain.Validate(environment)
	return result
}

// ValidateEnvironmentType reports whether the environment model accepts t,
// returning the model's own error - which names the accepted values - when it
// does not.
//
// The check goes through FromLegacyWorld with an empty world because lib/domain
// does not export its list of environment types. Repeating the list here would
// be the worse option: a type added there and forgotten here would be rejected
// by a tool that has no business having an opinion about it. The call is pure
// and its result is discarded.
func ValidateEnvironmentType(t domain.EnvironmentType) error {
	_, _, err := domain.FromLegacyWorld(state.World{}, t)
	return err
}

// worldId prefers the id in the document over the key it was loaded under.
// LoadWorlds() keys by exactly that id, so the two normally agree; they can
// differ for a map built from somewhere else, and then the document wins,
// because that is the id the conversion uses.
func worldId(key string, world *state.World) string {
	if world != nil && strings.TrimSpace(world.Id) != "" {
		return strings.TrimSpace(world.Id)
	}
	return strings.TrimSpace(key)
}

func worldName(world *state.World) string {
	if world == nil {
		return ""
	}
	return world.Name
}

func countZones(zones []domain.Zone) (zoneCount int, assetCount int, channelCount int) {
	for i := range zones {
		zoneCount++
		nestedZones, nestedAssets, nestedChannels := countZones(zones[i].Zones)
		zoneCount += nestedZones
		assetCount += nestedAssets
		channelCount += nestedChannels
		for _, asset := range zones[i].Assets {
			assetCount++
			channelCount += len(asset.Channels)
		}
	}
	return zoneCount, assetCount, channelCount
}
