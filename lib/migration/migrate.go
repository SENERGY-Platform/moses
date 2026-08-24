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
// environment model of lib/domain. Plan() is pure - it neither reads nor writes
// the database - so a migration of production data can be unit tested and the
// caller decides whether to print or execute the plan.
package migration

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/state"
)

// ErrNilWorld is reported rather than panicking, so one broken entry does not
// take down the rest of the migration.
var ErrNilWorld = errors.New("the legacy world is null")

// ErrNoLegacyId blocks the world instead of migrating it: FromLegacyWorld would
// generate a fresh uuid, which differs per run, so a second run would not
// recognise the world as migrated and would insert a duplicate.
var ErrNoLegacyId = errors.New("the legacy world has no id, so a repeated migration could not recognise it")

// ErrDuplicateEnvironmentId: Put() is keyed by id, so writing both would let
// the second silently replace the first.
var ErrDuplicateEnvironmentId = errors.New("two legacy worlds convert into the same environment id")

// WorldPlan is what the migration intends to do with one legacy world. Problems
// do not block a write; they are the work list for re-modelling change routines.
//
// An existing environment id is skipped, never overwritten: the stored document
// may carry runtime state, hand corrections of the kinds and types the
// conversion had to guess, and channel sources that replaced change routines.
// None of that exists in the legacy world, so a second conversion would discard
// it. A deliberate re-import deletes the environment first.
type WorldPlan struct {
	// WorldId is the world's own id, or the key it was loaded under when it has
	// none - blocking (ErrNoLegacyId), but still reportable.
	WorldId string

	// WorldName is the legacy name; Environment.Name may carry a substituted
	// placeholder instead.
	WorldName string

	// Environment is filled even for a skipped or blocked plan, so a dry run can
	// report what would have been written.
	Environment domain.Environment

	Problems []domain.Problem

	// Err is why this plan must not be written. Also set for a skipped world
	// whose conversion would not validate - information, not a failure, since
	// the stored environment is what runs.
	Err error

	Skip bool

	// SkipReason is empty exactly when Skip is false.
	SkipReason string
}

func (this WorldPlan) Writable() bool {
	return !this.Skip && this.Err == nil
}

// Blocked: a skipped world is not blocked, nothing is wrong with it.
func (this WorldPlan) Blocked() bool {
	return !this.Skip && this.Err != nil
}

func (this WorldPlan) Counts() (zones int, assets int, channels int) {
	return countZones(this.Environment.Zones)
}

// routineMarker matches the paths FromLegacyWorld builds for unmapped routines.
const routineMarker = "change_routines["

// UnmappedRoutines is why the legacy world has to be kept: their javascript is
// not in the converted document.
func (this WorldPlan) UnmappedRoutines() []domain.Problem {
	result := []domain.Problem{}
	for _, problem := range this.Problems {
		if strings.Contains(problem.Path, routineMarker) {
			result = append(result, problem)
		}
	}
	return result
}

func (this WorldPlan) OtherProblems() []domain.Problem {
	result := []domain.Problem{}
	for _, problem := range this.Problems {
		if !strings.Contains(problem.Path, routineMarker) {
			result = append(result, problem)
		}
	}
	return result
}

// Plan decides what to do with every legacy world. existingIds is queried by the
// caller; a world found in it is skipped (ref WorldPlan). The result is ordered
// by name then id, so two runs over the same data print the same report.
func Plan(worlds map[string]*state.World, envType domain.EnvironmentType, existingIds map[string]bool) []WorldPlan {
	keys := make([]string, 0, len(worlds))
	for key := range worlds {
		keys = append(keys, key)
	}
	// sorted before converting: the duplicate detection below depends on which
	// world is seen first, so the input order has to be total too.
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
	// environment id -> world id, so a collision can name both sides
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

	// by value, and FromLegacyWorld deep copies every state map, so the plan
	// shares nothing with the legacy document
	environment, problems, err := domain.FromLegacyWorld(*world, envType)
	if err != nil {
		// per world rather than returned: the report has to name what was skipped
		result.Err = err
		return result
	}
	result.Environment = environment
	if problems != nil {
		result.Problems = problems
	}

	if strings.TrimSpace(environment.Owner) == "" {
		// ListByOwner filters on this field, so an unowned environment is in
		// nobody's list. Usual cause is the input, not the data: World.Owner is
		// bson only, so a world from a json export never carries one.
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

	// validated for a skipped world too: a conversion that would not validate
	// means the legacy world and the stored environment have drifted apart
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
