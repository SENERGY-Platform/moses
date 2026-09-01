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

package domain

import (
	"fmt"
	"strings"
	"time"
)

// The timeline is how a measure that takes effect on a date is written down: one
// document, one environment, and a step in the series where the change lands.
// Everything it addresses stays a pure function of the instant, so the live
// simulation, a backfill and a history run see the same step.

// MaxTimelineChanges bounds the timeline of one environment. An imported
// document is untrusted input and must not be able to exhaust memory, which is
// the reason MaxNodes exists; indexing the changes is linear and cheap, so this
// is a bound on what may be stored rather than on what the runtime can afford.
const MaxTimelineChanges = 10000

// The timeline lives inside the range int64 nanoseconds can express, which is
// what a published reading is stamped with; the lower bound also catches the
// mistyped year that would otherwise pass as a perfectly valid instant.
var (
	minTimelineTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTimelineTime = time.Date(2262, 1, 1, 0, 0, 0, 0, time.UTC)
)

// DatedChange is one value that takes effect at an instant. From At on - the
// comparison is inclusive, so an instant exactly on At already reads the new
// value - Target carries Value; before the first change of a target the
// document's own inline value stands. There is no interpolation: a measure takes
// effect on a date, it does not fade in.
type DatedChange struct {
	// At must be a whole second. Every clock decision of the runtime is made on
	// the second grid and the store truncates to milliseconds, so a fraction
	// here would mean one instant in the document and another after a round trip.
	// It is compared through At.Unix(), which is free of the zone it was written
	// in.
	At time.Time `json:"at" bson:"at"`

	// Target names what changes, in one of the forms ParseTimelineTarget reads.
	Target string `json:"target" bson:"target"`

	// Value is numeric in v1: every field a target can name is a number, so an
	// interface{} here would be a format nothing reads.
	Value float64 `json:"value" bson:"value"`
}

// TimelineKind is what a target addresses.
type TimelineKind string

const (
	TimelineChannel       TimelineKind = "channel"
	TimelineContextSource TimelineKind = "context_source"
	TimelineContext       TimelineKind = "context"
)

// TimelineField is the parameter a target changes. The list is closed: a field
// that is not here is not governed by the timeline, and adding one means teaching
// the runtime to resolve it.
type TimelineField string

const (
	TimelineProfileBase   TimelineField = "profile.base"
	TimelineProfileSpread TimelineField = "profile.spread_percent"
	TimelineDatasetScale  TimelineField = "dataset.scale"
	TimelineStateValue    TimelineField = "schedule.states.value"
	TimelineStateSpread   TimelineField = "schedule.states.spread_percent"
	TimelineGateThreshold TimelineField = "schedule.gate.threshold"
	TimelineContextValue  TimelineField = "value"
)

// TimelineTarget is a parsed target. Every field is a string, so the whole
// struct is comparable and usable as the key of the runtime's index.
type TimelineTarget struct {
	Kind TimelineKind
	// Ref is the channel id, the context source key or the context key.
	Ref string
	// Field is the parameter that changes.
	Field TimelineField
	// State is the name of the schedule state, empty for every other field.
	State string
}

const (
	timelineChannelPrefix       = "channel."
	timelineContextSourcePrefix = "context_source."
	timelineContextPrefix       = "context."
	// timelineStatesSeparator is where a schedule state target is split. It is a
	// fixed separator rather than a field position because both the channel id
	// before it and the state name after it may carry dots.
	timelineStatesSeparator = ".schedule.states."
)

// ParseTimelineTarget reads one target of the closed v1 list.
//
// It parses suffix first, against the fixed field list, because the id or key in
// the middle may itself contain dots: what a target has fixed is its ending, not
// where the name stops. A schedule state is split at the first occurrence of
// ".schedule.states.", which leaves the dots to the state name - free text an
// author chooses - rather than to a channel id, which would have to contain that
// whole separator to be ambiguous at all.
func ParseTimelineTarget(target string) (TimelineTarget, error) {
	//context_source before context: the two prefixes differ in one character and
	//"context_source.x" does not start with "context.", but reading them in this
	//order keeps that from being something to notice
	if rest, ok := strings.CutPrefix(target, timelineContextSourcePrefix); ok {
		return parseTimelineSourceTarget(TimelineContextSource, rest, target)
	}
	if rest, ok := strings.CutPrefix(target, timelineChannelPrefix); ok {
		return parseTimelineChannelTarget(rest, target)
	}
	if key, ok := strings.CutPrefix(target, timelineContextPrefix); ok {
		if key == "" {
			return TimelineTarget{}, timelineTargetError(target)
		}
		return TimelineTarget{Kind: TimelineContext, Ref: key, Field: TimelineContextValue}, nil
	}
	return TimelineTarget{}, timelineTargetError(target)
}

// parseTimelineChannelTarget reads the part after "channel.". The schedule
// states separator decides first: a state name may end in ".profile", and
// reading such a target by its channel level suffix would silently address a
// channel whose id nobody wrote.
func parseTimelineChannelTarget(rest string, target string) (TimelineTarget, error) {
	if id, tail, found := strings.Cut(rest, timelineStatesSeparator); found {
		if id == "" {
			return TimelineTarget{}, timelineTargetError(target)
		}
		for _, candidate := range []struct {
			suffix string
			field  TimelineField
		}{
			{"value", TimelineStateValue},
			{"spread_percent", TimelineStateSpread},
		} {
			if name, ok := cutTimelineField(tail, candidate.suffix); ok {
				return TimelineTarget{Kind: TimelineChannel, Ref: id, Field: candidate.field, State: name}, nil
			}
		}
		return TimelineTarget{}, timelineTargetError(target)
	}
	if id, ok := cutTimelineField(rest, string(TimelineGateThreshold)); ok {
		return TimelineTarget{Kind: TimelineChannel, Ref: id, Field: TimelineGateThreshold}, nil
	}
	return parseTimelineSourceTarget(TimelineChannel, rest, target)
}

// parseTimelineSourceTarget reads the three fields a channel and a context source
// have in common - both carry a Source, and both may drive it from a profile or a
// dataset.
func parseTimelineSourceTarget(kind TimelineKind, rest string, target string) (TimelineTarget, error) {
	for _, field := range []TimelineField{TimelineProfileBase, TimelineProfileSpread, TimelineDatasetScale} {
		if ref, ok := cutTimelineField(rest, string(field)); ok {
			return TimelineTarget{Kind: kind, Ref: ref, Field: field}, nil
		}
	}
	return TimelineTarget{}, timelineTargetError(target)
}

// cutTimelineField takes "<ref>.<field>" apart. An empty ref is refused: a target
// naming no channel and no key addresses nothing at all.
func cutTimelineField(rest string, field string) (string, bool) {
	ref, ok := strings.CutSuffix(rest, "."+field)
	if !ok || ref == "" {
		return "", false
	}
	return ref, true
}

// timelineTargetError names every form there is, because the closed list is the
// whole grammar and an author who guessed wrong cannot look it up in the message
// otherwise.
func timelineTargetError(target string) error {
	return fmt.Errorf("unreadable timeline target %q, expected one of "+
		"channel.<id>.profile.base, channel.<id>.profile.spread_percent, channel.<id>.dataset.scale, "+
		"channel.<id>.schedule.states.<name>.value, channel.<id>.schedule.states.<name>.spread_percent, "+
		"channel.<id>.schedule.gate.threshold, "+
		"context_source.<key>.profile.base, context_source.<key>.profile.spread_percent, "+
		"context_source.<key>.dataset.scale, context.<key>", target)
}

// timelineSubject is what a target addresses, for a message that has to name it.
func timelineSubject(kind TimelineKind) string {
	if kind == TimelineContextSource {
		return "context source"
	}
	return "channel"
}
