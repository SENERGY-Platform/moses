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

package runtime

import (
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
)

// StateSnapshot is the live state of one environment at one instant.
//
// The state is a repo.StateChange and not a repo.RuntimeState on purpose: it is
// the shape SetState accepts, so a caller reads a value, changes it and sends
// the same shape back. What RuntimeState carries beyond it - the replay anchors
// and the approaches in flight - is bookkeeping of the runtime and would only
// invite a caller to write it back.
type StateSnapshot struct {
	State repo.StateChange

	// AsOf is the instant the values were read at, and the instant the values
	// with a time constant were resolved to. It is part of the answer rather
	// than something the reader stamps on arrival: a value that is on its way to
	// a set point means nothing without the moment it was read.
	AsOf time.Time
}

// Snapshot reads the live state of one running environment.
//
// It reports repo.ErrNotRunning for an id this runtime does not hold. That is
// deliberately not an empty snapshot: an environment that is running and has
// written nothing yet, and one that is not running at all, are the same three
// empty maps, and a reader has to be able to tell them apart.
//
// The values are copies. A caller mutating what it got must not reach the maps
// the scripts keep writing into - which is the same reason the flusher copies
// before it hands the state to the store.
func (this *Runtime) Snapshot(id string) (StateSnapshot, error) {
	//the env lookup and its generation are guarded by the runtime mux, the way
	//SetState reads them
	this.mux.RLock()
	env, running := this.envs[id]
	var gen *generation
	if running {
		gen = env.gen
	}
	this.mux.RUnlock()
	if !running {
		return StateSnapshot{}, repo.ErrNotRunning
	}

	env.mux.Lock()
	defer env.mux.Unlock()
	if env.removed {
		return StateSnapshot{}, repo.ErrNotRunning
	}
	//refused rather than served: advanceZone below moves the approaching values
	//to the wall clock, which for an environment standing at a past instant would
	//corrupt the run rather than report it
	if env.underHistory {
		return StateSnapshot{}, ErrHistoryRunning
	}

	//one instant for the whole snapshot: two zones resolved against two
	//different clocks would be a state the environment never had
	now := time.Now()
	//a zone value with a time constant is on its way to a set point and is only
	//written when it is read - exactly what jsZoneApi does before a script sees
	//it, so reading it here without advancing would report a stale value.
	//Advancing marks the environment dirty, so the flusher persists what was
	//resolved, the same as a script read does.
	//
	//Ranging over the map that advanceZone deletes from is safe: deleting during
	//a range is defined, and an entry deleted before it is reached was going to
	//be dropped anyway.
	for zoneId := range env.state.Approaching {
		env.advanceZone(zoneId, now)
	}

	//snapshot() is the deep copy the flusher already relies on
	state := env.snapshot()
	//the one place the timeline covers a value instead of replacing it: what the
	//stored map holds for a governed key is whatever was last written into it,
	//while every reader of that key inside the simulation sees the declared
	//value of this instant. Reporting the map would show a number nothing acts
	//on. Applied to the copy only, so nothing about the live state moves.
	if gen != nil {
		gen.timeline.overlayContext(state.Context, now)
	}
	return StateSnapshot{
		State: repo.StateChange{
			Context: state.Context,
			Zones:   state.Zones,
			Assets:  state.Assets,
		},
		AsOf: now,
	}, nil
}
