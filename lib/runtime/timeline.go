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
	"sort"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
)

// The timeline index is the document's dated changes made answerable: which
// value a governed parameter stands at at one instant. It is a pure function of
// the definition, built once per generation and read by all three execution
// paths - the live ticker, a backfill and a history run - which is what makes
// them see the same step rather than three that resemble each other.

// timelineIndex resolves one target to the value in effect at an instant.
//
// A nil index is the ordinary case, a document without a timeline, and every
// method below is nil safe: that is the short circuit which keeps such a
// document byte identical to what it produced before the field existed.
type timelineIndex struct {
	// changes holds the entries of one target sorted by instant, which is what
	// the binary search of valueAt needs.
	changes map[domain.TimelineTarget][]timelineChange

	// governedContext is the set of context keys the timeline governs, whether
	// or not a change of theirs has taken effect yet: the read-only layer holds
	// from the first tick, not from the first change, or a value set in between
	// would jump back when the change arrives.
	governedContext map[string]bool
}

// timelineChange is one entry, reduced to what a lookup needs. The instant is
// kept as whole seconds because that is what every clock decision of the runtime
// compares on, and it is free of the zone the document was written in.
type timelineChange struct {
	atUnix int64
	value  float64
}

// newTimelineIndex builds the index of one definition. It reports what it cannot
// use rather than dropping it silently, the way newGeneration does: validation
// refuses all of these on the way in, so anything found here came from a hand
// written document or from a future version of the format.
func newTimelineIndex(def domain.Environment) *timelineIndex {
	if len(def.Timeline) == 0 {
		return nil
	}
	result := &timelineIndex{
		changes:         map[domain.TimelineTarget][]timelineChange{},
		governedContext: map[string]bool{},
	}
	for _, change := range def.Timeline {
		target, err := domain.ParseTimelineTarget(change.Target)
		if err != nil {
			util.Logger.Warn("unreadable timeline target, this dated change does nothing",
				attributes.ErrorKey, err, "environment", def.Id, "target", change.Target)
			continue
		}
		if math.IsNaN(change.Value) || math.IsInf(change.Value, 0) {
			//dropped rather than carried: such a value would not merely be wrong
			//for this parameter, it would turn every later reading of the channel
			//into a NaN and every total above it with them
			util.Logger.Warn("timeline value is not a finite number, this dated change does nothing",
				"environment", def.Id, "target", change.Target, "value", change.Value)
			continue
		}
		if target.Kind == domain.TimelineContext && !result.governedContext[target.Ref] {
			if _, driven := def.ContextSources[target.Ref]; driven {
				//validation refuses this pair, so the document bypassed the api.
				//The source keeps writing the raw state on every tick while the
				//layer answers every read, so the key reads as two different
				//values depending on who asks - loud rather than left to be
				//noticed in a series. Reported here, in document order and once
				//per key, rather than from a walk over the map afterwards.
				util.Logger.Warn("a context key is both driven by a context source and governed by the timeline, the source writes a value nothing reads",
					"environment", def.Id, "key", target.Ref)
			}
			result.governedContext[target.Ref] = true
		}
		result.changes[target] = append(result.changes[target],
			timelineChange{atUnix: change.At.Unix(), value: change.Value})
	}
	if len(result.changes) == 0 {
		//nothing usable is left: the nil index is the short circuit, and handing
		//out an empty one would only cost a map lookup per tick forever
		return nil
	}
	for target := range result.changes {
		entries := result.changes[target]
		//stable, so that two entries validation would have refused - one target
		//at one instant, twice - keep document order and the later one wins the
		//lookup below, rather than the pair deciding it between them
		sort.SliceStable(entries, func(a int, b int) bool { return entries[a].atUnix < entries[b].atUnix })
	}
	return result
}

// valueAt is the value of one target at at, and whether the timeline has taken
// effect for it yet.
//
// The comparison is inclusive: a change dated exactly at at is in effect, which
// is what "from this date on" means to whoever wrote it. False means the inline
// value of the document still stands.
func (this *timelineIndex) valueAt(target domain.TimelineTarget, at time.Time) (float64, bool) {
	if this == nil {
		return 0, false
	}
	entries := this.changes[target]
	if len(entries) == 0 {
		return 0, false
	}
	//whole seconds on both sides: the instant of a tick carries nanoseconds a
	//document cannot express, and comparing those would make a change dated on
	//the second land one tick late
	atUnix := at.Unix()
	//the first entry that lies strictly after at; everything before it has taken
	//effect, so the one before it is the value that stands
	next := sort.Search(len(entries), func(i int) bool { return entries[i].atUnix > atUnix })
	if next == 0 {
		return 0, false
	}
	return entries[next-1].value, true
}

// effectiveProfile is the profile of one channel or context source at at: the
// inline one with base and spread replaced wherever the timeline has taken
// effect. Everything else about the profile - the factors, cumulative - is not
// governed and comes through untouched.
func (this *timelineIndex) effectiveProfile(kind domain.TimelineKind, ref string, profile domain.ProfileSource, at time.Time) domain.ProfileSource {
	if this == nil {
		return profile
	}
	if base, governed := this.valueAt(domain.TimelineTarget{Kind: kind, Ref: ref, Field: domain.TimelineProfileBase}, at); governed {
		profile.Base = base
	}
	if spread, governed := this.valueAt(domain.TimelineTarget{Kind: kind, Ref: ref, Field: domain.TimelineProfileSpread}, at); governed {
		profile.SpreadPercent = spread
	}
	return profile
}

// effectiveDataset is the dataset source of one channel or context source at at.
// Only the scale is governed: the reference, the resampling and the anchor decide
// what is replayed and from where, and changing those mid-replay would be a
// different series rather than a step in this one.
func (this *timelineIndex) effectiveDataset(kind domain.TimelineKind, ref string, source domain.DatasetSource, at time.Time) domain.DatasetSource {
	if this == nil {
		return source
	}
	if scale, governed := this.valueAt(domain.TimelineTarget{Kind: kind, Ref: ref, Field: domain.TimelineDatasetScale}, at); governed {
		source.Scale = scale
	}
	return source
}

// effectiveScheduleState is one step of a programme at at. The state is
// addressed by its name, so the value of a state that is not running right now
// is resolved when the programme reaches it. Name, duration and state writes are
// untouched: only what the step publishes is governed.
func (this *timelineIndex) effectiveScheduleState(channelId string, state domain.ScheduleState, at time.Time) domain.ScheduleState {
	if this == nil {
		return state
	}
	target := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: channelId, State: state.Name}
	target.Field = domain.TimelineStateValue
	if value, governed := this.valueAt(target, at); governed {
		state.Value = value
	}
	target.Field = domain.TimelineStateSpread
	if spread, governed := this.valueAt(target, at); governed {
		state.SpreadPercent = spread
	}
	return state
}

// effectiveGateThreshold is the threshold a schedule's gate compares against at
// at. It takes effect at the next evaluation of that gate, like every other
// change to a gate does.
func (this *timelineIndex) effectiveGateThreshold(channelId string, threshold float64, at time.Time) float64 {
	if this == nil {
		return threshold
	}
	target := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: channelId, Field: domain.TimelineGateThreshold}
	if value, governed := this.valueAt(target, at); governed {
		return value
	}
	return threshold
}

// effectiveContext is the declared value of one context key at at. The bool is
// false for a key the timeline does not govern, and for a governed one before
// its first change, where the inline value in the live state still stands.
func (this *timelineIndex) effectiveContext(key string, at time.Time) (float64, bool) {
	if this == nil {
		return 0, false
	}
	return this.valueAt(domain.TimelineTarget{Kind: domain.TimelineContext, Ref: key, Field: domain.TimelineContextValue}, at)
}

// governsContext reports whether the document declares this context key as a
// function of time. It is true from the start rather than from the first change:
// a key whose first change lies ahead is still not one anybody may set, or the
// value somebody wrote would be thrown away the moment the change arrives.
func (this *timelineIndex) governsContext(key string) bool {
	return this != nil && this.governedContext[key]
}

// overlayContext writes the declared value of every governed key into target -
// the read side of the layer for a caller that reads the whole context at once
// rather than one key at a time. A key whose first change has not arrived is
// left alone, so the inline value that is already there stands.
func (this *timelineIndex) overlayContext(target map[string]interface{}, at time.Time) {
	if this == nil {
		return
	}
	for key := range this.governedContext {
		if value, governed := this.effectiveContext(key, at); governed {
			target[key] = value
		}
	}
}

// contextValue reads one context key through the read-only layer: a governed key
// answers with what the document declares for this instant, whatever the live
// state happens to hold. It must be called with env.mux held, like every other
// read of the state maps.
func (this *Runtime) contextValue(env *environment, gen *generation, key string, at time.Time) float64 {
	//guarded like jsContextStateApi guards it, although no caller today can hand
	//in a nil generation: the two are the read side of one layer, and one of them
	//panicking where the other returns is the kind of asymmetry a later caller
	//walks into
	if gen == nil {
		return numericOrZero(env.contextStates()[key])
	}
	if value, governed := gen.timeline.effectiveContext(key, at); governed {
		return value
	}
	return numericOrZero(env.contextStates()[key])
}
