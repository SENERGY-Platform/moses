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
	"math"
	"strconv"
	"time"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// A schedule source is a declared machine programme: named states, each held
// for a duration and publishing a value. Everything above executeSchedule in
// this file is pure - a function of the definition, the seed, the persisted
// anchor and the clock - for the same reason profileValue is: the same seed and
// the same clock have to produce the same programme across restarts, and a walk
// that depended on how many ticks had happened before could not.

// maxScheduleCycleWalk bounds the walk from the anchor to now, guarding
// against a pathological gap - state stored years ago and started again, or a
// clock jump - rather than the normal case, where every evaluation only walks
// the gap since the last one. Beyond the bound the remaining gap is folded
// into the cycle the walk stopped at, so the programme keeps running instead
// of burning a core to catch up under the environment mutex.
const maxScheduleCycleWalk = 1 << 20

// schedulePosition is where a programme stands at one instant.
type schedulePosition struct {
	// index is the state of source.States that is currently running.
	index int

	// cycle is the absolute number of the cycle it is running in, counted from
	// the anchor the run was first created with - not from its rolled forward
	// StartUnix. It is what the per-cycle duration draw is taken on.
	cycle int64

	// consumedCycles and consumedSeconds are the whole cycles between
	// run.StartUnix and now, and what they were worth in seconds. They are the
	// roll-forward: adding them to the anchor moves it to the start of the
	// current cycle without moving the position or changing a single draw.
	consumedCycles  int64
	consumedSeconds int64

	// held says a run_once programme has finished and is standing in its last
	// state. Nothing is consumed then: there is no next cycle to advance into.
	held bool
}

// scheduleAt walks the programme from its anchor to nowUnix. Draws are salted
// so a gated schedule varies by run.PassUnix (fixed once per pass, so shifts
// differ but a running pass is not redrawn as StartUnix rolls forward), while
// a gate-less schedule uses a constant salt. Both also draw on the absolute
// cycle number, which is what lets StartUnix roll forward by whole cycles
// without moving any later draw.
func scheduleAt(source domain.ScheduleSource, seed int64, channelId string, run repo.ScheduleRun, nowUnix int64) schedulePosition {
	if len(source.States) == 0 {
		//the generation refuses such a channel, so this is defensive only
		return schedulePosition{}
	}
	if nowUnix < run.StartUnix {
		//the clock went backwards, or the anchor lies in the future. Clamping to
		//the first state is the one answer that is not made up: the programme has
		//not started yet as far as the anchor is concerned.
		return schedulePosition{index: 0, cycle: run.CycleOffset}
	}

	keys := scheduleDrawKeys(channelId, scheduleSalt(source, run), len(source.States))
	durations := make([]int64, len(source.States))
	remaining := nowUnix - run.StartUnix

	// Every cycle is the same length unless a state varies its duration, and
	// then the whole walk is one division. It is not an optimisation of the
	// common case only: it is what keeps a gate that was left open for years
	// exact rather than folded into the walk bound below.
	if !scheduleVaries(source) {
		total := scheduleCycle(source, seed, keys, run.CycleOffset, durations)
		cycles := remaining / total
		within := remaining - cycles*total
		if source.RunOnce && cycles > 0 {
			return schedulePosition{index: len(source.States) - 1, cycle: run.CycleOffset, held: true}
		}
		return schedulePosition{
			index:           scheduleIndexWithin(durations, within),
			cycle:           run.CycleOffset + cycles,
			consumedCycles:  cycles,
			consumedSeconds: cycles * total,
		}
	}

	position := schedulePosition{cycle: run.CycleOffset}
	for walked := 0; walked < maxScheduleCycleWalk; walked++ {
		total := scheduleCycle(source, seed, keys, position.cycle, durations)
		if remaining < total {
			position.index = scheduleIndexWithin(durations, remaining)
			return position
		}
		if source.RunOnce {
			//a single pass is over, and the last state is what a finished job
			//leaves behind: a forklift that charged stays charged
			position.index = len(source.States) - 1
			position.held = true
			return position
		}
		remaining -= total
		position.consumedSeconds += total
		position.consumedCycles++
		position.cycle++
	}
	//the bound was hit. What is left is folded into the cycle the walk stopped
	//at, so the machine keeps cycling with that cycle's durations; the caller
	//persists what was consumed, so the next evaluation starts that much closer
	//and the gap closes over the next few of them.
	total := scheduleCycle(source, seed, keys, position.cycle, durations)
	position.index = scheduleIndexWithin(durations, remaining%total)
	return position
}

// scheduleSalt is what separates the draws of one run from another's. See
// scheduleAt for why the two shapes differ.
//
// It reads PassUnix and never StartUnix: StartUnix moves with the roll-forward,
// and a salt that followed it would redraw the whole running pass on every
// evaluation - the drift this source is built to not have.
func scheduleSalt(source domain.ScheduleSource, run repo.ScheduleRun) int64 {
	if source.Gate != nil {
		return run.PassUnix
	}
	return 0
}

// scheduleRollsForward says whether the anchor of a run may be advanced by the
// cycles it has consumed. Every cycling programme may, since its draws hang on
// PassUnix and the absolute cycle number, neither of which the roll-forward
// touches; only a run_once one may not, since it has no second cycle to
// advance into. Skipping this would leave a schedule whose gate never closes
// walking from its rising edge without bound, under the environment mutex
// every other source shares.
func scheduleRollsForward(source domain.ScheduleSource) bool {
	return !source.RunOnce
}

// scheduleVaries reports whether any state draws its duration per cycle.
func scheduleVaries(source domain.ScheduleSource) bool {
	for i := range source.States {
		if source.States[i].DurationSpreadPercent > 0 {
			return true
		}
	}
	return false
}

// scheduleCycle fills the durations of one absolute cycle and returns their
// sum. The sum is at least one second per state, which is what bounds the walk
// in scheduleAt and what keeps the division in its fast path defined.
func scheduleCycle(source domain.ScheduleSource, seed int64, keys []string, cycle int64, durations []int64) int64 {
	total := int64(0)
	for i := range source.States {
		durations[i] = scheduleStateDuration(source.States[i], seed, keys[i], cycle)
		total += durations[i]
	}
	return total
}

// scheduleStateDuration is how long one state lasts in one cycle, drawn with
// spreadDraw on the absolute cycle number rather than a time slot, so the
// duration is decided once for the whole step. The floor of one second keeps a
// cycle from ever having zero length, which the walk above divides by; the
// ceiling guards against a float that does not fit an int64, which is
// implementation defined and can land on the most negative value. Both clamps
// are unreachable through the api, where validation demands a duration
// between one second and a year.
func scheduleStateDuration(state domain.ScheduleState, seed int64, key string, cycle int64) int64 {
	seconds := float64(state.DurationSeconds)
	if state.DurationSpreadPercent > 0 {
		seconds *= 1 + (state.DurationSpreadPercent/100)*spreadDraw(seed, key, cycle)
	}
	if seconds > float64(domain.MaxScheduleDurationSeconds) {
		return domain.MaxScheduleDurationSeconds
	}
	//NaN falls through the comparison above and out of the conversion as
	//something below one, which the floor catches
	drawn := int64(math.Round(seconds))
	if drawn < 1 {
		return 1
	}
	return drawn
}

// scheduleDrawKeys builds the hash key of every state once per evaluation
// rather than once per cycle: the walk uses them again for every cycle it
// covers, and a string built inside that loop would be the only allocation in
// it.
//
// The salt is part of the key and not of the slot, so that two runs of the same
// channel cannot land on the same draw sequence by their salt and cycle
// happening to add up to the same number.
func scheduleDrawKeys(channelId string, salt int64, count int) []string {
	keys := make([]string, count)
	prefix := channelId + "|" + strconv.FormatInt(salt, 10) + "|"
	for i := range keys {
		keys[i] = prefix + strconv.Itoa(i)
	}
	return keys
}

// scheduleIndexWithin finds the state a phase inside one cycle falls into.
func scheduleIndexWithin(durations []int64, within int64) int {
	for i, duration := range durations {
		if within < duration {
			return i
		}
		within -= duration
	}
	//the caller only ever passes a phase shorter than the cycle, so this is
	//reached for a rounding nobody can produce; the last state is the honest
	//answer for "the cycle is nearly over"
	return len(durations) - 1
}

// scheduleValue is what the channel publishes while one state runs.
//
// The spread is the profile's, per time slot rather than per cycle: a state
// that held a perfectly flat number would give a channel publishing on change
// nothing to see between two state transitions, and a real machine's power draw
// is not flat while it runs. stepSeconds is the span one computation stands
// for, so the value is stable within one tick window.
func scheduleValue(state domain.ScheduleState, seed int64, channelId string, stepSeconds int64, t time.Time) float64 {
	value := state.Value
	if state.SpreadPercent > 0 {
		slot := t.Unix()
		if stepSeconds > 0 {
			slot = t.Unix() / stepSeconds
		}
		value *= 1 + (state.SpreadPercent/100)*spreadDraw(seed, channelId, slot)
	}
	return value
}

// executeSchedule advances the programme and publishes the value of the state
// it stands in. Like executeProfile it takes the environment mutex for the
// whole run, send included: the asset state it writes - the name of the running
// state, the values that state declares - is state like any other, and a
// formula on the same asset reads it under the same mutex.
func (this *Runtime) executeSchedule(env *environment, gen *generation, binding channelBinding, send func(value interface{}), now time.Time) {
	source := *binding.channel.Source.Schedule

	env.mux.Lock()
	defer env.mux.Unlock()

	run, open := this.scheduleRun(env, gen, binding, source, now)
	if !open {
		//a closed gate is the machine standing still, and it has to say so in
		//every value it writes: the name, every state write of every state, and
		//the published reading. Leaving the last values standing would keep an
		//air demand or a power draw alive through the night.
		this.writeScheduleStates(env, binding, source, domain.ScheduleClosedState, nil)
		send(0.0)
		return
	}

	position := scheduleAt(source, gen.def.Seed, binding.channel.Id, run, now.Unix())
	if scheduleRollsForward(source) && position.consumedCycles > 0 {
		//the anchor moves to the start of the cycle that is running now; the
		//cycles it skipped keep being counted, but position, value and every
		//future duration draw are unchanged (see scheduleAt), so this is
		//bookkeeping, not a state change. A gated run rolls too, since its draws
		//hang on PassUnix rather than StartUnix - otherwise a gate that never
		//closes would walk from its rising edge again on every evaluation.
		run.StartUnix += position.consumedSeconds
		run.CycleOffset += position.consumedCycles
		this.storeScheduleRun(env, binding.channel.Id, run)
	}

	//the value of this step as the timeline has it at this instant; the walk
	//above is untouched by it, since durations are not governed
	state := gen.timeline.effectiveScheduleState(binding.channel.Id, source.States[position.index], now)
	this.writeScheduleStates(env, binding, source, state.Name, state.StateWrites)
	send(scheduleValue(state, gen.def.Seed, binding.channel.Id, binding.stepSeconds, now))
}

// scheduleRun returns the run of this channel and whether its gate is open,
// creating and restarting it as the gate demands. It must be called with
// env.mux held.
//
// The rising edge is the point of the gate: a schedule that merely paused
// while closed would resume mid-state, so a shift break would just continue
// the previous shift instead of starting the morning peak fresh.
func (this *Runtime) scheduleRun(env *environment, gen *generation, binding channelBinding, source domain.ScheduleSource, now time.Time) (repo.ScheduleRun, bool) {
	id := binding.channel.Id
	run, known := env.state.ScheduleRuns[id]

	if source.Gate == nil {
		if known && run.Open {
			return run, true
		}
		if !known {
			run = repo.ScheduleRun{StartUnix: now.Unix(), PassUnix: now.Unix()}
		}
		//a run left behind by a gated version of this channel keeps its anchor, so
		//the programme carries on from where it stood rather than restarting. Its
		//durations do change, since a gate-less run draws on a constant salt
		//instead of PassUnix - that is the document having changed, not drift, and
		//the position a reader sees is kept.
		run.Open = true
		this.storeScheduleRun(env, id, run)
		return run, true
	}

	//the same leniency resolveInput has: a key nothing has written yet, or one
	//carrying something that is not a number, reads as 0 - which is a closed
	//gate. A gate that defaulted to open would run every declared machine of a
	//site through a shift nobody scheduled.
	//
	//Both sides go through the timeline: the key through the read-only layer,
	//the threshold through its own target. A dated threshold takes effect at the
	//next evaluation, which is the gate semantics that was already there.
	threshold := gen.timeline.effectiveGateThreshold(id, source.Gate.Threshold, now)
	open := this.contextValue(env, gen, source.Gate.ContextKey, now) > threshold
	switch {
	case open && (!known || !run.Open):
		//the rising edge is a new pass: the anchor, the cycle counter and the
		//salt of the draws all start here
		run = repo.ScheduleRun{StartUnix: now.Unix(), PassUnix: now.Unix(), Open: true}
		this.storeScheduleRun(env, id, run)
	case !open && !known:
		//nothing has ever run: there is no anchor worth storing, and the next
		//rise is an edge whether or not one is written here
		return repo.ScheduleRun{}, false
	case !open && run.Open:
		run.Open = false
		this.storeScheduleRun(env, id, run)
	case open && run.PassUnix == 0:
		//an open run stored before the salt was split off the anchor: StartUnix
		//is still the instant its pass began, so adopting it as PassUnix keeps
		//the shift's duration sequence. Healed here rather than as a fallback in
		//scheduleSalt, since StartUnix rolls forward from the next evaluation on
		//and a fallback would follow it - the drift PassUnix exists to prevent.
		run.PassUnix = run.StartUnix
		this.storeScheduleRun(env, id, run)
	}
	return run, open
}

// storeScheduleRun writes the run back and marks the environment dirty for it.
// A write that changes nothing is not a change: an unconditional dirty here
// would make every schedule channel of every site rewrite its whole state
// document on every flush interval, forever.
func (this *Runtime) storeScheduleRun(env *environment, channelId string, run repo.ScheduleRun) {
	if env.state.ScheduleRuns == nil {
		env.state.ScheduleRuns = map[string]repo.ScheduleRun{}
	}
	if previous, known := env.state.ScheduleRuns[channelId]; known && previous == run {
		return
	}
	env.state.ScheduleRuns[channelId] = run
	env.dirty = true
}

// writeScheduleStates puts the name of the running state and the values that
// state declares into the asset state. It must be called with env.mux held.
//
// writes is nil for a closed gate, which is not the same as a state without
// declared writes: both end up writing 0 for every key the schedule knows, and
// that union is the point. A key only the running state declares would
// otherwise keep the value it had while that state ran - an air demand that
// stands all night because nothing in the document ever says it stopped.
func (this *Runtime) writeScheduleStates(env *environment, binding channelBinding, source domain.ScheduleSource, name string, writes map[string]float64) {
	states := env.assetStates(binding.asset.id)
	if setStateValue(states, source.StateKey, name) {
		env.dirty = true
	}
	for i := range source.States {
		for key := range source.States[i].StateWrites {
			value := 0.0
			if declared, ok := writes[key]; ok {
				value = declared
			}
			if setStateValue(states, key, value) {
				env.dirty = true
			}
		}
	}
}

// setStateValue writes a state value and reports whether anything changed.
//
// The comparison is what keeps a schedule from marking its environment dirty on
// every single evaluation. A machine that stands in one state for twenty
// minutes would otherwise have the whole state document of its site written out
// on every flush interval for those twenty minutes, with nothing in it moving.
func setStateValue(states map[string]interface{}, key string, value interface{}) bool {
	if previous, exists := states[key]; exists && sameStateValue(previous, value) {
		return false
	}
	states[key] = value
	return true
}

// sameStateValue compares a value a schedule writes - a name or a number -
// against what is stored. It is typed rather than a bare == on two interfaces
// because a state key may already hold a map or a slice from initial_states,
// and comparing those with == is a panic and not a false.
func sameStateValue(previous interface{}, value interface{}) bool {
	switch typed := value.(type) {
	case string:
		text, ok := previous.(string)
		return ok && text == typed
	case float64:
		//through asFloat, so a value decoded from bson as an int counts as equal
		//to the float it stands for instead of being rewritten on every tick
		number, ok := asFloat(previous)
		return ok && number == typed
	}
	return false
}
