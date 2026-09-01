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
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// timelineAt is a whole second well inside the accepted range, which is what
// every case below dates its change to unless it is testing the instant itself.
var timelineAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// timelineEnvironment carries one of every source a target can name: a profile
// channel, a dataset channel, a gated schedule with a dotted state name, a
// profile context source, a dataset context source and a plain context key.
func timelineEnvironment(changes ...DatedChange) Environment {
	return Environment{
		Id: "e1", Name: "Werk", Type: IndustrialSite, Owner: "o",
		Context: map[string]interface{}{"shift": float64(0), "price": 0.32},
		ContextSources: map[string]Source{
			"outside": {Kind: SourceProfile, IntervalSeconds: 300, Profile: &ProfileSource{Base: 12}},
			"sun": {Kind: SourceDataset, IntervalSeconds: 300, Dataset: &DatasetSource{
				Origin: OriginFile, Ref: "d1", Resample: ResampleLinear, Anchor: AnchorLoop}},
		},
		Timeline: changes,
		Zones: []Zone{{Id: "z1", Name: "Halle", Type: ZoneHall,
			Assets: []Asset{{Id: "a1", Name: "Fräse", Kind: AssetMachine,
				ExternalTypeId: "urn:infai:ses:device-type:x",
				Channels: []Channel{
					{Id: "ch.power", Name: "Wirkleistung", Direction: Sensor, IntervalSeconds: 60,
						Source: Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 230, SpreadPercent: 5}}},
					{Id: "ch-replay", Name: "Lastgang", Direction: Sensor, IntervalSeconds: 60,
						Source: Source{Kind: SourceDataset, Dataset: &DatasetSource{
							Origin: OriginFile, Ref: "d1", Resample: ResampleHold, Anchor: AnchorLoop}}},
					{Id: "ch-meter", Name: "Zähler", Direction: Sensor, IntervalSeconds: 60,
						Source: Source{Kind: SourceDataset, Dataset: &DatasetSource{
							Origin: OriginFile, Ref: "d1", Resample: ResampleHold, Anchor: AnchorLoop, Cumulative: true}}},
					{Id: "ch-prog", Name: "Programm", Direction: Sensor, IntervalSeconds: 60,
						Source: Source{Kind: SourceSchedule, Schedule: &ScheduleSource{
							StateKey: "programm",
							Gate:     &ScheduleGate{ContextKey: "shift"},
							States: []ScheduleState{
								{Name: "setup", DurationSeconds: 300, Value: 2000},
								{Name: "run.fast", DurationSeconds: 1800, Value: 9000, SpreadPercent: 5},
							},
						}}},
				}}}}},
	}
}

func change(target string, value float64) DatedChange {
	return DatedChange{At: timelineAt, Target: target, Value: value}
}

// expectTimelineProblem asserts that a change is refused, at the path it belongs
// to: an editor marks the offending field by that path, so a right message under
// a wrong path is not usable.
func expectTimelineProblem(t *testing.T, change DatedChange, path string, fragment string) {
	t.Helper()
	err := Validate(timelineEnvironment(change))
	invalid, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected a validation error for %+v, got %v", change, err)
	}
	for _, problem := range invalid.Problems {
		if problem.Path == path && strings.Contains(problem.Message, fragment) {
			return
		}
	}
	t.Errorf("expected a problem at %q mentioning %q for %+v, got %v", path, fragment, change, invalid.Problems)
}

// ---------------------------------------------------------------------------
// the grammar
// ---------------------------------------------------------------------------

func TestParseTimelineTarget(t *testing.T) {
	tests := []struct {
		target string
		want   TimelineTarget
	}{
		{"channel.ch-1.profile.base", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineProfileBase}},
		{"channel.ch-1.profile.spread_percent", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineProfileSpread}},
		{"channel.ch-1.dataset.scale", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineDatasetScale}},
		{"channel.ch-1.schedule.gate.threshold", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineGateThreshold}},
		{"channel.ch-1.schedule.states.run.value", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineStateValue, State: "run"}},
		{"channel.ch-1.schedule.states.run.spread_percent", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineStateSpread, State: "run"}},
		{"context_source.outside.profile.base", TimelineTarget{Kind: TimelineContextSource, Ref: "outside", Field: TimelineProfileBase}},
		{"context_source.outside.dataset.scale", TimelineTarget{Kind: TimelineContextSource, Ref: "outside", Field: TimelineDatasetScale}},
		{"context.shift", TimelineTarget{Kind: TimelineContext, Ref: "shift", Field: TimelineContextValue}},

		// the reason the parse reads the suffix first: an id and a state name may
		// both carry dots, and neither ends the target
		{"channel.urn:infai:ses:x.1.profile.base", TimelineTarget{Kind: TimelineChannel, Ref: "urn:infai:ses:x.1", Field: TimelineProfileBase}},
		{"channel.ch-1.schedule.states.run.fast.value", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineStateValue, State: "run.fast"}},
		{"context_source.a.b.profile.spread_percent", TimelineTarget{Kind: TimelineContextSource, Ref: "a.b", Field: TimelineProfileSpread}},
		// a state name that ends in ".profile" is why the states separator is read
		// before the channel level suffixes
		{"channel.ch-1.schedule.states.x.profile.value", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineStateValue, State: "x.profile"}},
		// a state named "value" still parses: the suffix is cut, the rest is the name
		{"channel.ch-1.schedule.states.value.value", TimelineTarget{Kind: TimelineChannel, Ref: "ch-1", Field: TimelineStateValue, State: "value"}},
		// "context_source." is not "context.", so the key is not "_source.outside..."
		{"context_source.x.dataset.scale", TimelineTarget{Kind: TimelineContextSource, Ref: "x", Field: TimelineDatasetScale}},
	}
	for _, test := range tests {
		got, err := ParseTimelineTarget(test.target)
		if err != nil {
			t.Errorf("ParseTimelineTarget(%q) failed: %v", test.target, err)
			continue
		}
		if got != test.want {
			t.Errorf("ParseTimelineTarget(%q) = %+v, want %+v", test.target, got, test.want)
		}
	}
}

func TestParseTimelineTargetRefusesEverythingElse(t *testing.T) {
	refused := []string{
		"",
		"channel",
		"channel.",
		"channel.ch-1",
		"channel.ch-1.profile",
		"channel.ch-1.profile.spread",
		"channel.ch-1.source.interval_seconds",
		"channel.ch-1.dataset.ref",
		"channel.ch-1.schedule.states.run.duration_seconds",
		"channel.ch-1.schedule.states..value",
		"channel..schedule.states.run.value",
		".profile.base",
		"context.",
		"context",
		"context_source.outside",
		"context_source..profile.base",
		"zone.z1.state.x",
		"CHANNEL.ch-1.profile.base",
	}
	for _, target := range refused {
		if got, err := ParseTimelineTarget(target); err == nil {
			t.Errorf("ParseTimelineTarget(%q) was accepted as %+v", target, got)
		}
	}
}

// ---------------------------------------------------------------------------
// acceptance
// ---------------------------------------------------------------------------

func TestValidateAcceptsEveryTargetForm(t *testing.T) {
	accepted := []DatedChange{
		change("channel.ch.power.profile.base", 180),
		change("channel.ch.power.profile.spread_percent", 0),
		change("channel.ch-replay.dataset.scale", 1.5),
		change("channel.ch-prog.schedule.states.setup.value", 1500),
		change("channel.ch-prog.schedule.states.run.fast.value", 7000),
		change("channel.ch-prog.schedule.states.run.fast.spread_percent", 2),
		change("channel.ch-prog.schedule.gate.threshold", 0.5),
		change("context_source.outside.profile.base", 9),
		change("context_source.outside.profile.spread_percent", 3),
		change("context_source.sun.dataset.scale", 0.8),
		change("context.price", 0.41),
	}
	for _, accepted := range accepted {
		if err := Validate(timelineEnvironment(accepted)); err != nil {
			t.Errorf("%q has to be storable: %v", accepted.Target, err)
		}
	}
	//a scale of zero is "unscaled", exactly as an omitted inline scale is, so it
	//is a value and not an unset field
	if err := Validate(timelineEnvironment(change("channel.ch-replay.dataset.scale", 0))); err != nil {
		t.Errorf("a scale of zero has to be storable: %v", err)
	}
	//and all of them together, which is also the case that pins that a dotted
	//channel id and a dotted state name do not collide with each other
	if err := Validate(timelineEnvironment(accepted...)); err != nil {
		t.Errorf("a document carrying every target form has to be storable: %v", err)
	}
}

// A change in the future is the case the timeline exists for: a measure that is
// planned. Nothing may refuse it for lying ahead of the clock.
func TestValidateAcceptsAChangeInTheFutureAndAnUnsortedTimeline(t *testing.T) {
	future := DatedChange{At: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), Target: "channel.ch.power.profile.base", Value: 100}
	if err := Validate(timelineEnvironment(future)); err != nil {
		t.Errorf("a planned measure has to be storable: %v", err)
	}

	//document order is free: the index sorts, so an editor appending a correction
	//with an earlier date must not have to rewrite the list
	unsorted := timelineEnvironment(
		DatedChange{At: timelineAt.Add(48 * time.Hour), Target: "channel.ch.power.profile.base", Value: 100},
		DatedChange{At: timelineAt, Target: "channel.ch.power.profile.base", Value: 200},
		DatedChange{At: timelineAt.Add(24 * time.Hour), Target: "channel.ch.power.profile.base", Value: 150},
	)
	if err := Validate(unsorted); err != nil {
		t.Errorf("an unsorted timeline has to be storable: %v", err)
	}
}

// The instant is compared through Unix(), so the zone it was written in decides
// nothing. Two changes that name the same second in two zones are one slot.
func TestValidateComparesTheInstantAcrossZones(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no zone database here: %v", err)
	}
	env := timelineEnvironment(
		DatedChange{At: timelineAt, Target: "channel.ch.power.profile.base", Value: 100},
		DatedChange{At: timelineAt.In(berlin), Target: "channel.ch.power.profile.base", Value: 200},
	)
	if err := Validate(env); err == nil || !strings.Contains(err.Error(), "already changes") {
		t.Errorf("the same second in two zones is one slot, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// refusals
// ---------------------------------------------------------------------------

func TestValidateRefusesBrokenDatedChanges(t *testing.T) {
	//the instant
	expectTimelineProblem(t, DatedChange{Target: "context.price", Value: 1}, "timeline[0].at", "must be set")
	expectTimelineProblem(t, DatedChange{
		At: timelineAt.Add(500 * time.Millisecond), Target: "context.price", Value: 1,
	}, "timeline[0].at", "whole second")
	expectTimelineProblem(t, DatedChange{
		At: time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC), Target: "context.price", Value: 1,
	}, "timeline[0].at", "must lie between")
	expectTimelineProblem(t, DatedChange{
		At: time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), Target: "context.price", Value: 1,
	}, "timeline[0].at", "must lie between")

	//the target
	expectTimelineProblem(t, change("channel.ch.power.profile.mean", 1), "timeline[0].target", "unreadable timeline target")
	expectTimelineProblem(t, change("channel.nope.profile.base", 1), "timeline[0].target", "does not exist in this environment")
	expectTimelineProblem(t, change("context_source.nope.profile.base", 1), "timeline[0].target", "no context source of this environment writes")
	expectTimelineProblem(t, change("context.nope", 1), "timeline[0].target", "not declared in context")

	//the field against the source that is actually there
	expectTimelineProblem(t, change("channel.ch-replay.profile.base", 1), "timeline[0].target", "carries no profile to change")
	expectTimelineProblem(t, change("channel.ch.power.dataset.scale", 1), "timeline[0].target", "carries no dataset to change")
	expectTimelineProblem(t, change("channel.ch.power.schedule.gate.threshold", 1), "timeline[0].target", "runs no schedule to change")
	expectTimelineProblem(t, change("channel.ch-prog.schedule.states.nope.value", 1), "timeline[0].target", "no state named")
	expectTimelineProblem(t, change("context_source.sun.profile.base", 1), "timeline[0].target", "carries no profile to change")
	expectTimelineProblem(t, change("context_source.outside.dataset.scale", 1), "timeline[0].target", "carries no dataset to change")
	//scaling a meter reading from an instant on restates everything it counted
	expectTimelineProblem(t, change("channel.ch-meter.dataset.scale", 2), "timeline[0].target", "is cumulative")

	//the value
	expectTimelineProblem(t, change("channel.ch.power.profile.base", math.NaN()), "timeline[0].value", "finite")
	expectTimelineProblem(t, change("channel.ch.power.profile.base", math.Inf(1)), "timeline[0].value", "finite")
	expectTimelineProblem(t, change("channel.ch.power.profile.spread_percent", -1), "timeline[0].value", "must not be negative")
	expectTimelineProblem(t, change("channel.ch-prog.schedule.states.setup.spread_percent", -1), "timeline[0].value", "must not be negative")
	expectTimelineProblem(t, change("context.price", math.NaN()), "timeline[0].value", "finite")
	expectTimelineProblem(t, change("channel.ch-prog.schedule.gate.threshold", math.Inf(-1)), "timeline[0].value", "finite")
	//a negative threshold is a legitimate shape - a gate on a temperature - and
	//is not refused with the spreads
	if err := Validate(timelineEnvironment(change("channel.ch-prog.schedule.gate.threshold", -5))); err != nil {
		t.Errorf("a gate on a negative threshold has to be storable: %v", err)
	}
}

// A gate threshold on a schedule without a gate changes a field that is not
// there: the runtime would resolve it and nothing would ever read it.
func TestValidateRefusesAGateThresholdWithoutAGate(t *testing.T) {
	env := timelineEnvironment(change("channel.ch-prog.schedule.gate.threshold", 1))
	env.Zones[0].Assets[0].Channels[3].Source.Schedule.Gate = nil
	//"off" is only refused as a state name while a gate is there
	err := Validate(env)
	if err == nil || !strings.Contains(err.Error(), "has no gate") {
		t.Errorf("expected a problem about the missing gate, got %v", err)
	}
}

// A key a context source drives is refused as a dated target even when it is
// also declared statically: the source writes it on every tick, so the dated
// value would stand until the next one and then disappear.
func TestValidateRefusesADatedChangeOnASourceDrivenContextKey(t *testing.T) {
	env := timelineEnvironment(change("context.outside", 1))
	env.Context["outside"] = float64(0)
	err := Validate(env)
	if err == nil || !strings.Contains(err.Error(), "driven by a context source") {
		t.Errorf("expected the source-driven key to be refused, got %v", err)
	}
}

func TestValidateRefusesTwoValuesForOneInstant(t *testing.T) {
	env := timelineEnvironment(
		change("channel.ch.power.profile.base", 100),
		change("channel.ch.power.profile.base", 200),
	)
	err := Validate(env)
	if err == nil || !strings.Contains(err.Error(), "already changes") {
		t.Fatalf("expected the duplicate to be refused, got %v", err)
	}
	//reported at the later one: the first claim keeps the slot, as everywhere else
	invalid := err.(*ValidationError)
	if invalid.Problems[0].Path != "timeline[1]" {
		t.Errorf("expected the problem at timeline[1], got %v", invalid.Problems)
	}

	//the same target at two instants is the ordinary case and stays accepted
	fine := timelineEnvironment(
		change("channel.ch.power.profile.base", 100),
		DatedChange{At: timelineAt.Add(time.Second), Target: "channel.ch.power.profile.base", Value: 200},
	)
	if err := Validate(fine); err != nil {
		t.Errorf("two changes of one target at two instants have to be storable: %v", err)
	}

	//and two targets at one instant likewise
	twoTargets := timelineEnvironment(
		change("channel.ch.power.profile.base", 100),
		change("channel.ch.power.profile.spread_percent", 2),
	)
	if err := Validate(twoTargets); err != nil {
		t.Errorf("two targets at one instant have to be storable: %v", err)
	}
}

func TestValidateBoundsTheTimeline(t *testing.T) {
	changes := make([]DatedChange, MaxTimelineChanges+1)
	for i := range changes {
		changes[i] = DatedChange{
			At: timelineAt.Add(time.Duration(i) * time.Second), Target: "channel.ch.power.profile.base", Value: 1,
		}
	}
	err := Validate(timelineEnvironment(changes...))
	if err == nil || !strings.Contains(err.Error(), "at most 10000 changes") {
		t.Fatalf("expected the bound to be enforced, got %v", err)
	}
	//and the refusal is one problem, not ten thousand: the list is untrusted
	//input and the response must not be the same denial of service
	invalid := err.(*ValidationError)
	if len(invalid.Problems) != 1 {
		t.Errorf("expected exactly one problem, got %d", len(invalid.Problems))
	}

	//exactly at the bound is still accepted
	if err := Validate(timelineEnvironment(changes[:MaxTimelineChanges]...)); err != nil {
		t.Errorf("a timeline of exactly %d changes has to be storable: %v", MaxTimelineChanges, err)
	}
}

// The problem list has to be the same on every save, or a caller diffing two
// validation responses sees changes that are not there.
func TestTheTimelineProblemListIsDeterministic(t *testing.T) {
	env := timelineEnvironment(
		change("channel.nope.profile.base", 1),
		change("context.nope", 1),
		change("channel.ch.power.profile.spread_percent", -1),
		change("channel.ch-replay.profile.base", 1),
	)
	first := Validate(env).(*ValidationError)
	for i := 0; i < 20; i++ {
		again := Validate(env).(*ValidationError)
		if !reflect.DeepEqual(first.Problems, again.Problems) {
			t.Fatalf("the problem list changed between two runs:\n%v\n%v", first.Problems, again.Problems)
		}
	}
	if len(first.Problems) != 4 {
		t.Errorf("expected one problem per broken change, got %v", first.Problems)
	}
}

// A document without a timeline has to keep validating exactly as it did, which
// is what makes the field additive for every stored document.
func TestAnEnvironmentWithoutATimelineIsUnaffected(t *testing.T) {
	if err := Validate(timelineEnvironment()); err != nil {
		t.Errorf("the fixture without a timeline has to be valid: %v", err)
	}
}
