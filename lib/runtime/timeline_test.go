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
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/dataset"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// timelineKnick is the instant the dated changes of these tests land on, well
// inside the past so nothing here races the wall clock.
var timelineKnick = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func datedChange(target string, at time.Time, value float64) domain.DatedChange {
	return domain.DatedChange{At: at, Target: target, Value: value}
}

// indexOf builds the index the way a generation does, from a definition that
// carries nothing but the timeline: everything below the index is a pure
// function of the changes themselves.
func indexOf(changes ...domain.DatedChange) *timelineIndex {
	return newTimelineIndex(domain.Environment{Id: "env-timeline", Timeline: changes})
}

// ---------------------------------------------------------------------------
// the lookup
// ---------------------------------------------------------------------------

// TestTheLookupTakesEffectAtTheInstantItself walks the four positions a lookup
// can fall into. The exact hit is the one that matters: "from this date on" has
// to include the date, and a search that answered strictly-before would move
// every measure one evaluation late.
func TestTheLookupTakesEffectAtTheInstantItself(t *testing.T) {
	first := timelineKnick
	second := timelineKnick.Add(time.Hour)
	index := indexOf(
		//written in the wrong order on purpose: document order is free and the
		//index sorts
		datedChange("channel.ch-1.profile.base", second, 300),
		datedChange("channel.ch-1.profile.base", first, 200),
	)
	target := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: "ch-1", Field: domain.TimelineProfileBase}

	tests := []struct {
		name     string
		at       time.Time
		want     float64
		governed bool
	}{
		{"a second before the first change the inline value still stands", first.Add(-time.Second), 0, false},
		{"exactly on the first change it has taken effect", first, 200, true},
		{"a nanosecond into the first second is the same second", first.Add(time.Nanosecond), 200, true},
		{"between the two changes the first one stands", first.Add(30 * time.Minute), 200, true},
		{"exactly on the second change it has taken effect", second, 300, true},
		{"after both the last one stands", second.Add(10 * 365 * 24 * time.Hour), 300, true},
	}
	for _, test := range tests {
		got, governed := index.valueAt(target, test.at)
		if governed != test.governed || (governed && got != test.want) {
			t.Errorf("%s: valueAt = (%v, %v), want (%v, %v)", test.name, got, governed, test.want, test.governed)
		}
	}

	//a target nothing changes is not governed, whatever the timeline holds for
	//its neighbours
	other := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: "ch-2", Field: domain.TimelineProfileBase}
	if _, governed := index.valueAt(other, second); governed {
		t.Error("a target with no change of its own must not be governed")
	}
}

// Both sides of the comparison are whole seconds. Validation demands a whole
// second for the instant, so this is what a hand written document gets: the
// fraction is dropped rather than deciding anything, and the change takes effect
// at the start of the second it names. Comparing nanoseconds instead would make
// the answer depend on a fraction the document cannot even express through the
// api, and would sit far closer to the point where int64 nanoseconds run out
// than the year 2262 the format allows.
func TestTheLookupComparesWholeSeconds(t *testing.T) {
	fractional := timelineKnick.Add(400 * time.Millisecond)
	index := indexOf(domain.DatedChange{At: fractional, Target: "channel.ch-1.profile.base", Value: 200})
	target := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: "ch-1", Field: domain.TimelineProfileBase}

	//the start of that second, which lies before the fraction the entry carries
	if value, governed := index.valueAt(target, timelineKnick); !governed || value != 200 {
		t.Errorf("a fractional instant has to take effect at the start of its second, got (%v, %v)", value, governed)
	}
	//and a tick inside the previous second is still before it
	if _, governed := index.valueAt(target, timelineKnick.Add(-time.Millisecond)); governed {
		t.Error("the second before the change must not be governed")
	}
	//a tick carrying a fraction of its own lands in the same second either way
	if value, governed := index.valueAt(target, timelineKnick.Add(900*time.Millisecond)); !governed || value != 200 {
		t.Errorf("a tick with a fraction has to read the same second, got (%v, %v)", value, governed)
	}
}

// The nil index is the short circuit that keeps a document without a timeline
// byte identical to what it produced before the field existed.
func TestANilIndexAnswersWithTheInlineValues(t *testing.T) {
	var index *timelineIndex
	if index != nil {
		t.Fatal("the zero value of the index has to be nil")
	}
	if newTimelineIndex(domain.Environment{Id: "e"}) != nil {
		t.Error("a document without a timeline has to produce no index at all")
	}

	profile := domain.ProfileSource{Base: 230, SpreadPercent: 5}
	if got := index.effectiveProfile(domain.TimelineChannel, "ch-1", profile, timelineKnick); !reflect.DeepEqual(got, profile) {
		t.Errorf("a nil index changed the profile to %+v", got)
	}
	replay := domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Scale: 3}
	if got := index.effectiveDataset(domain.TimelineChannel, "ch-1", replay, timelineKnick); got != replay {
		t.Errorf("a nil index changed the dataset to %+v", got)
	}
	state := domain.ScheduleState{Name: "run", Value: 9000, SpreadPercent: 5}
	if got := index.effectiveScheduleState("ch-1", state, timelineKnick); got.Value != 9000 || got.SpreadPercent != 5 {
		t.Errorf("a nil index changed the state to %+v", got)
	}
	if got := index.effectiveGateThreshold("ch-1", 0.5, timelineKnick); got != 0.5 {
		t.Errorf("a nil index changed the threshold to %v", got)
	}
	if _, governed := index.effectiveContext("price", timelineKnick); governed {
		t.Error("a nil index must govern no context key")
	}
	if index.governsContext("price") {
		t.Error("a nil index must govern no context key")
	}
	context := map[string]interface{}{"price": 0.3}
	index.overlayContext(context, timelineKnick)
	if context["price"] != 0.3 {
		t.Errorf("a nil index wrote into the context: %v", context)
	}
}

// A target the runtime cannot read, and a value it cannot compute with, are both
// refused by validation - so both got here past the api, and both are dropped
// with a word rather than carried into every later reading.
func TestTheIndexSkipsWhatItCannotUse(t *testing.T) {
	index := indexOf(
		datedChange("channel.ch-1.profile.mean", timelineKnick, 1),
		datedChange("channel.ch-1.profile.base", timelineKnick, 200),
	)
	target := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: "ch-1", Field: domain.TimelineProfileBase}
	if value, governed := index.valueAt(target, timelineKnick); !governed || value != 200 {
		t.Errorf("the readable change has to survive its neighbour, got (%v, %v)", value, governed)
	}

	//nothing readable at all is the nil index again, not an empty one
	if got := indexOf(datedChange("nonsense", timelineKnick, 1)); got != nil {
		t.Errorf("a timeline of nothing usable has to produce no index, got %+v", got)
	}

	//a value that is not a number is dropped too: carried, it would not merely be
	//wrong for this parameter but would turn every later reading of the channel
	//into a NaN, and every total above it with them
	for _, broken := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		index := indexOf(
			datedChange("channel.ch-1.profile.base", timelineKnick, broken),
			datedChange("channel.ch-1.profile.spread_percent", timelineKnick, 5),
		)
		if value, governed := index.valueAt(target, timelineKnick); governed {
			t.Errorf("a value of %v has to be dropped, got %v", broken, value)
		}
		//and its readable neighbour survives it
		spread := domain.TimelineTarget{Kind: domain.TimelineChannel, Ref: "ch-1", Field: domain.TimelineProfileSpread}
		if _, governed := index.valueAt(spread, timelineKnick); !governed {
			t.Errorf("the readable change has to survive a broken value next to it")
		}
	}
}

// A context key that is both source-driven and governed is refused by
// validation, so a document carrying it bypassed the api: the source keeps
// writing the raw state while the layer answers every read, and the key then
// reads as two different values depending on who asks. The index still governs
// it - the layer is what the readers go through either way.
func TestTheIndexStillGovernsASourceDrivenContextKey(t *testing.T) {
	def := domain.Environment{
		Id:      "env-timeline-collision",
		Context: map[string]interface{}{"outside": 0.0},
		ContextSources: map[string]domain.Source{"outside": {
			Kind: domain.SourceProfile, IntervalSeconds: 300, Profile: &domain.ProfileSource{Base: 12},
		}},
		Timeline: []domain.DatedChange{
			datedChange("context.outside", timelineKnick, 25),
			//a second change of the same key, so the report stays at one per key
			datedChange("context.outside", timelineKnick.Add(time.Hour), 26),
		},
	}
	index := newTimelineIndex(def)
	if !index.governsContext("outside") {
		t.Error("the key has to stay governed, whatever else writes it")
	}
	if value, governed := index.effectiveContext("outside", timelineKnick); !governed || value != 25 {
		t.Errorf("the declared value has to stand, got (%v, %v)", value, governed)
	}
}

// ---------------------------------------------------------------------------
// the spread does not move with the value
// ---------------------------------------------------------------------------

// TestABaseKnickShiftsTheProfileWithoutRedrawingItsSpread is the property a
// dated change lives or dies by: the draw hangs on the seed, the channel id and
// the time slot alone, so doubling the base at one instant doubles the value at
// that instant exactly. Both bases are powers of two, which makes the ratio an
// exact float comparison rather than one with an epsilon nobody can defend.
func TestABaseKnickShiftsTheProfileWithoutRedrawingItsSpread(t *testing.T) {
	const channelId = "ch-1"
	const seed = int64(4711)
	const step = int64(3600)
	index := indexOf(datedChange("channel."+channelId+".profile.base", timelineKnick, 256))
	inline := domain.ProfileSource{Base: 128, SpreadPercent: 20}

	//one instant for both, so the spread slot is trivially the same one and the
	//only difference between the two values is the base
	at := timelineKnick.Add(17 * time.Minute)
	before := profileValue(index.effectiveProfile(domain.TimelineChannel, channelId, inline, timelineKnick.Add(-time.Second)), seed, channelId, step, at)
	after := profileValue(index.effectiveProfile(domain.TimelineChannel, channelId, inline, timelineKnick), seed, channelId, step, at)

	if before == 0 {
		t.Fatal("the fixture produced no value at all")
	}
	if after != 2*before {
		t.Errorf("the knick doubled the base, so the value has to double exactly: %v and %v", before, after)
	}
}

// And a spread knick scales the variation around the same draw rather than
// drawing again, which is what keeps the series continuous through it.
func TestASpreadKnickScalesTheSameDraw(t *testing.T) {
	const channelId = "ch-1"
	const seed = int64(4711)
	const step = int64(3600)
	index := indexOf(datedChange("channel."+channelId+".profile.spread_percent", timelineKnick, 40))
	inline := domain.ProfileSource{Base: 230, SpreadPercent: 10}

	at := timelineKnick.Add(17 * time.Minute)
	after := profileValue(index.effectiveProfile(domain.TimelineChannel, channelId, inline, timelineKnick), seed, channelId, step, at)

	//recomputed with the same operations profileValue makes, so the comparison
	//is exact: a redrawn spread would land somewhere else entirely
	draw := spreadDraw(seed, channelId, at.Unix()/step)
	want := 230 * (1 + (40.0/100)*draw)
	if after != want {
		t.Errorf("the spread knick has to scale the draw that was already there: got %v, want %v", after, want)
	}
	//and the draw is a real one, or the assertion above would hold for anything
	if draw == 0 {
		t.Fatal("the fixture drew exactly zero, so it proves nothing")
	}
}

// ---------------------------------------------------------------------------
// a cumulative profile bends, it does not jump
// ---------------------------------------------------------------------------

// A meter reading is the integral of the rate, so a change of the rate is a
// change of the slope: the reading itself must stay continuous, or every
// consumer of that series reads a consumption that never happened.
func TestACumulativeProfileChangesItsSlopeWithoutAJump(t *testing.T) {
	const id = "env-timeline-cumulative"
	const step = int64(600)
	from := timelineKnick.Add(-2 * time.Hour)
	to := timelineKnick.Add(2 * time.Hour)

	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), step,
		domain.ProfileSource{Base: 120, Cumulative: true}))
	def.Timeline = []domain.DatedChange{datedChange("channel.ch-1.profile.base", timelineKnick, 360)}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	gen := newGeneration(def, nil)
	channel := backfillChannels(def)[0]

	counter := float64(0)
	values := []float64{}
	instants := []time.Time{}
	for at := from; !at.After(to); at = at.Add(time.Duration(step) * time.Second) {
		value, ok := rt.backfillValue(gen, channel, nil, from.Unix(), at, &counter, true, step)
		if !ok {
			t.Fatalf("the meter produced no reading at %v", at)
		}
		values = append(values, value)
		instants = append(instants, at)
	}

	//the rate before and after, as a step of the meter over one interval
	slow := 120 * float64(step) / 3600
	fast := 360 * float64(step) / 3600
	sawFast := false
	for i := 1; i < len(values); i++ {
		got := values[i] - values[i-1]
		//the step of an instant is the rate that holds at that instant, so the
		//first fast step is the one landing exactly on the knick
		want := slow
		if !instants[i].Before(timelineKnick) {
			want = fast
			sawFast = true
		}
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("the meter step at %v was %v, expected %v", instants[i], got, want)
		}
		if got <= 0 {
			t.Fatalf("a meter reading went backwards at %v", instants[i])
		}
	}
	if !sawFast {
		t.Fatal("the window never reached the knick, so the test proves nothing")
	}
}

// ---------------------------------------------------------------------------
// the context read-only layer
// ---------------------------------------------------------------------------

// governedEnvironment is a site whose electricity price is a declared function
// of time: a static context entry with a dated change on it.
func governedEnvironment(id string, changes ...domain.DatedChange) domain.Environment {
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 3600, flatProfile(230, 0)))
	def.Context = map[string]interface{}{"price": 0.30}
	def.Timeline = changes
	return def
}

// A script may read a governed key and gets what the document declares for the
// instant of its run. Setting it is dropped, and reported once per key rather
// than once per tick.
func TestAScriptReadsAGovernedContextKeyAndCannotWriteIt(t *testing.T) {
	def := governedEnvironment("env-governed",
		datedChange("context.price", timelineKnick, 0.42),
		datedChange("context.other", timelineKnick, 1))
	def.Context["other"] = float64(0)
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.seed(gen, timelineKnick.Add(-time.Hour))

	api := jsContextStateApi(env, gen, timelineKnick.Add(time.Hour))
	get := api["get"].(func(field string) interface{})
	set := api["set"].(func(field string, value interface{}))

	if got := get("price"); got != 0.42 {
		t.Errorf("a governed key has to read as the declared value of this instant, got %v", got)
	}
	//and the live map is untouched by the read: get must not seed or rewrite a
	//key the document decides
	if env.state.Context["price"] != 0.30 {
		t.Errorf("reading a governed key wrote into the state: %v", env.state.Context["price"])
	}

	set("price", 99.0)
	if env.state.Context["price"] != 0.30 {
		t.Errorf("a script wrote a governed key: %v", env.state.Context["price"])
	}
	if got := get("price"); got != 0.42 {
		t.Errorf("the declared value has to stand after a refused write, got %v", got)
	}
	//one entry per key is what makes the refusal one log line per key: a second
	//write of the same key finds the key already there and says nothing
	set("price", 98.0)
	if len(env.timelineWarned) != 1 || !env.timelineWarned["price"] {
		t.Errorf("expected exactly one reported key, got %v", env.timelineWarned)
	}
	set("other", 5.0)
	if len(env.timelineWarned) != 2 {
		t.Errorf("a second governed key has to be reported on its own, got %v", env.timelineWarned)
	}

	//an ungoverned key behaves exactly as it always did, seeding included
	set("free", 7.0)
	if env.state.Context["free"] != 7.0 {
		t.Errorf("an ungoverned key must still be writable, got %v", env.state.Context["free"])
	}
	if got := get("fresh"); got != 0 || env.state.Context["fresh"] != 0 {
		t.Errorf("an ungoverned key has to keep seeding on get, got %v and %v", got, env.state.Context["fresh"])
	}
}

// Before the first change of a governed key the inline value stands, and the
// script sees it without the key being seeded a second time.
func TestAGovernedKeyReadsItsInlineValueBeforeTheFirstChange(t *testing.T) {
	def := governedEnvironment("env-governed-early", datedChange("context.price", timelineKnick, 0.42))
	gen := newGeneration(def, nil)
	env := &environment{id: def.Id, state: repo.RuntimeState{EnvironmentId: def.Id}}
	env.seed(gen, timelineKnick.Add(-2*time.Hour))

	api := jsContextStateApi(env, gen, timelineKnick.Add(-time.Hour))
	if got := api["get"].(func(field string) interface{})("price"); got != 0.30 {
		t.Errorf("before the first change the inline value stands, got %v", got)
	}
}

// seed resolves the governed keys against the instant it is given, which is what
// separates the two starts: the live simulation seeds the value of now, a
// history run the value of its own window start. Seeding from the wall clock
// would put the future into the first tick of a run.
func TestSeedResolvesGovernedKeysAgainstTheInstantItIsGiven(t *testing.T) {
	def := governedEnvironment("env-governed-seed",
		datedChange("context.price", timelineKnick, 0.42),
		datedChange("context.price", timelineKnick.Add(24*time.Hour), 0.55))
	gen := newGeneration(def, nil)

	tests := []struct {
		name string
		at   time.Time
		want interface{}
	}{
		{"before the first change the inline value is seeded", timelineKnick.Add(-time.Second), 0.30},
		{"exactly on the first change its value is seeded", timelineKnick, 0.42},
		{"the window start of a run decides, not the wall clock", timelineKnick.Add(time.Hour), 0.42},
		{"after the second change its value is seeded", timelineKnick.Add(48 * time.Hour), 0.55},
	}
	for _, test := range tests {
		env := &environment{id: def.Id, state: repo.RuntimeState{EnvironmentId: def.Id}}
		env.seed(gen, test.at)
		if env.state.Context["price"] != test.want {
			t.Errorf("%s: seeded %v, want %v", test.name, env.state.Context["price"], test.want)
		}
	}

	//the definition is never written to, whatever a seed resolved
	if def.Context["price"] != 0.30 {
		t.Errorf("seeding rewrote the definition: %v", def.Context["price"])
	}
}

// The state endpoint refuses a governed key that a change would actually move,
// and names it. A value equal to the one the reading direction hands out is not
// a move and goes through - see the round trip test below.
func TestSetStateRefusesAGovernedContextKey(t *testing.T) {
	def := governedEnvironment("env-governed-set", datedChange("context.price", timelineKnick, 0.42))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), &fakePublisher{})

	err := rt.SetState(def.Id, repo.StateChange{Context: map[string]interface{}{"price": 0.99}})
	governed := &repo.TimelineGovernedError{}
	if err == nil || !errors.As(err, &governed) {
		t.Fatalf("expected the governed key to be refused, got %v", err)
	}
	if len(governed.Keys) != 1 || governed.Keys[0] != "price" {
		t.Errorf("the offending key has to be named, got %v", governed.Keys)
	}

	//an ungoverned key of the same change is still applied on its own
	if err := rt.SetState(def.Id, repo.StateChange{Context: map[string]interface{}{"free": 1.0}}); err != nil {
		t.Errorf("an ungoverned key has to stay settable: %v", err)
	}
}

// TestTheStateRoundTripSurvivesAGovernedKey is the pair of endpoints working as
// documented: a client reads the state, changes one key and sends the whole
// thing back. The governed key it did not touch comes back with the value the
// read handed out, and refusing that would break every such client for a change
// that changes nothing.
func TestTheStateRoundTripSurvivesAGovernedKey(t *testing.T) {
	def := governedEnvironment("env-governed-roundtrip",
		//in effect already, and a second one far in the future: a planned change
		//must not make the endpoint refuse a round trip today
		datedChange("context.price", timelineKnick, 0.42),
		datedChange("context.price", time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC), 0.90))
	def.Context["free"] = 1.0
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), &fakePublisher{})

	read, err := rt.Snapshot(def.Id)
	if err != nil {
		t.Fatalf("unable to read the state: %v", err)
	}
	if read.State.Context["price"] != 0.42 {
		t.Fatalf("the read has to hand out the declared value, got %v", read.State.Context["price"])
	}

	//sent back untouched, exactly as it was read
	if err = rt.SetState(def.Id, read.State); err != nil {
		t.Errorf("the unchanged round trip has to be accepted: %v", err)
	}
	//and with a neighbouring key edited, which is what a client actually does
	read.State.Context["free"] = 2.0
	if err = rt.SetState(def.Id, read.State); err != nil {
		t.Errorf("editing a neighbouring key has to be accepted: %v", err)
	}
	if got, _ := rt.Snapshot(def.Id); got.State.Context["free"] != 2.0 {
		t.Errorf("the neighbouring key was not applied, got %v", got.State.Context["free"])
	}

	//a number that arrived as an integer is the same value, not a move: json and
	//bson both hand out whole numbers in shapes of their own
	def2 := governedEnvironment("env-governed-int", datedChange("context.price", timelineKnick, 5))
	rt2 := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def2), newFakeStates(), &fakePublisher{})
	for _, value := range []interface{}{5.0, int64(5), int(5), float32(5)} {
		if err := rt2.SetState(def2.Id, repo.StateChange{Context: map[string]interface{}{"price": value}}); err != nil {
			t.Errorf("%T(%v) is the declared value and has to be accepted: %v", value, value, err)
		}
	}
	//and one that is not, whatever its shape, is still a move
	if err := rt2.SetState(def2.Id, repo.StateChange{Context: map[string]interface{}{"price": int64(6)}}); err == nil {
		t.Error("a different value has to be refused whatever type it arrives as")
	}
}

// What the timeline governs is a property of the definition, so what has already
// been reported about it belongs to the generation that is going away. Without
// the reset a timeline that was removed and added again would stay silent
// forever, on the strength of a line about a document that no longer exists.
func TestTheGovernedKeyWarningIsForgottenOnAReload(t *testing.T) {
	def := governedEnvironment("env-governed-reload", datedChange("context.price", timelineKnick, 0.42))
	store := newFakeEnvironments(def)
	rt := startRuntime(t, testConfig(time.Hour), store, newFakeStates(), &fakePublisher{})

	rt.mux.RLock()
	env := rt.envs[def.Id]
	rt.mux.RUnlock()

	env.mux.Lock()
	env.warnTimelineGoverned("price")
	reported := len(env.timelineWarned)
	env.mux.Unlock()
	if reported != 1 {
		t.Fatalf("the fixture reported nothing, so the test proves nothing: %d", reported)
	}

	rt.Reload(def.Id)

	env.mux.Lock()
	defer env.mux.Unlock()
	if len(env.timelineWarned) != 0 {
		t.Errorf("the new generation has to start with nothing reported, got %v", env.timelineWarned)
	}
}

// A change that is wrong in two ways is answered once, naming both: fixing it
// one round trip per mistake is what collecting the problems exists to avoid.
func TestSetStateNamesUnknownIdsAndGovernedKeysTogether(t *testing.T) {
	def := governedEnvironment("env-governed-both", datedChange("context.price", timelineKnick, 0.42))
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), &fakePublisher{})

	err := rt.SetState(def.Id, repo.StateChange{
		Context: map[string]interface{}{"price": 0.99},
		Zones:   map[string]map[string]interface{}{"no-zone": {"temperature": 20.0}},
	})
	if err == nil {
		t.Fatal("expected the change to be refused")
	}
	unknown := &repo.UnknownIdsError{}
	governed := &repo.TimelineGovernedError{}
	if !errors.As(err, &unknown) || !errors.As(err, &governed) {
		t.Fatalf("expected both refusals to be findable in %v", err)
	}
	//and both have to be in the message, which is what reaches the caller
	if !strings.Contains(err.Error(), "no-zone") || !strings.Contains(err.Error(), "price") {
		t.Errorf("expected both to be named, got %q", err.Error())
	}
}

// The snapshot is the one place the layer covers a value instead of replacing
// it: the stored map holds whatever was last written into it, and the answer has
// to show what the simulation actually acts on.
func TestTheSnapshotShowsTheDeclaredValueWithoutTouchingTheState(t *testing.T) {
	def := governedEnvironment("env-governed-snapshot", datedChange("context.price", timelineKnick, 0.42))
	//an hour long flush interval: the assertion is about the answer, not about
	//what a flush in between might have written
	rt := startRuntime(t, testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), &fakePublisher{})

	//a value nothing may set, put into the live map by hand: it is what the map
	//would hold after a flush of an older document, and the reader must not see it
	rt.mux.RLock()
	env := rt.envs[def.Id]
	rt.mux.RUnlock()
	env.mux.Lock()
	env.state.Context["price"] = 99.0
	env.mux.Unlock()

	got, err := rt.Snapshot(def.Id)
	if err != nil {
		t.Fatalf("unable to read the snapshot: %v", err)
	}
	if got.State.Context["price"] != 0.42 {
		t.Errorf("the snapshot has to show the declared value, got %v", got.State.Context["price"])
	}
	//the overlay runs on the copy: the live map is left exactly as it was
	env.mux.Lock()
	live := env.state.Context["price"]
	env.mux.Unlock()
	if live != 99.0 {
		t.Errorf("the overlay wrote into the live state: %v", live)
	}
}

// ---------------------------------------------------------------------------
// a gate threshold takes effect at the next evaluation
// ---------------------------------------------------------------------------

func TestAGateThresholdKnickTakesEffectAtTheNextEvaluation(t *testing.T) {
	const id = "env-timeline-gate"
	source := shortSchedule(
		domain.ScheduleState{Name: "a", DurationSeconds: 60, Value: 1},
		domain.ScheduleState{Name: "b", DurationSeconds: 60, Value: 2},
	)
	source.Gate = &domain.ScheduleGate{ContextKey: "shift"}
	def := testEnvironment(id, scheduleChannel("ch-1", serviceRefOf(id), 10, source))
	def.Context = map[string]interface{}{"shift": 1.0}
	def.Timeline = []domain.DatedChange{
		datedChange("channel.ch-1.schedule.gate.threshold", timelineKnick, 2),
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	gen := newGeneration(def, nil)
	env := &environment{id: id, state: repo.RuntimeState{
		EnvironmentId: id,
		Context:       map[string]interface{}{"shift": 1.0},
	}}
	binding := channelBinding{channel: def.Zones[0].Assets[0].Channels[0]}

	//the calendar says 1 throughout; only the threshold moves
	if _, open := rt.scheduleRun(env, gen, binding, source, timelineKnick.Add(-time.Second)); !open {
		t.Error("a shift of 1 is above the inline threshold of 0, so the gate has to be open")
	}
	if _, open := rt.scheduleRun(env, gen, binding, source, timelineKnick); open {
		t.Error("from the knick on the threshold is 2, so a shift of 1 no longer opens the gate")
	}
	//and the evaluation after it stays closed rather than flapping
	if _, open := rt.scheduleRun(env, gen, binding, source, timelineKnick.Add(time.Minute)); open {
		t.Error("the raised threshold has to keep holding")
	}
}

// A schedule state is addressed by its name, so the value of a step is resolved
// when the programme reaches it - the duration of the step is not governed and
// the walk is untouched.
func TestAScheduleStateKnickChangesTheValueAndNotTheProgramme(t *testing.T) {
	index := indexOf(
		datedChange("channel.ch-1.schedule.states.run.value", timelineKnick, 5000),
		datedChange("channel.ch-1.schedule.states.run.spread_percent", timelineKnick, 1),
	)
	state := domain.ScheduleState{Name: "run", DurationSeconds: 1800, Value: 9000, SpreadPercent: 5,
		StateWrites: map[string]float64{"air_demand": 120}}

	before := index.effectiveScheduleState("ch-1", state, timelineKnick.Add(-time.Second))
	if before.Value != 9000 || before.SpreadPercent != 5 {
		t.Errorf("before the knick the inline step stands, got %+v", before)
	}
	after := index.effectiveScheduleState("ch-1", state, timelineKnick)
	if after.Value != 5000 || after.SpreadPercent != 1 {
		t.Errorf("from the knick on the declared step applies, got %+v", after)
	}
	if after.Name != "run" || after.DurationSeconds != 1800 || after.StateWrites["air_demand"] != 120 {
		t.Errorf("only the published value of a step is governed, got %+v", after)
	}
	//a step of another name is not touched by the change of this one
	other := index.effectiveScheduleState("ch-1", domain.ScheduleState{Name: "setup", Value: 2000}, timelineKnick)
	if other.Value != 2000 {
		t.Errorf("the change of one state reached another, got %+v", other)
	}
}

// A scale of zero means unscaled, exactly as an omitted inline scale does, so a
// change back to it is a change and not an unset field.
func TestADatasetScaleKnickIncludingBackToUnscaled(t *testing.T) {
	index := indexOf(
		datedChange("channel.ch-1.dataset.scale", timelineKnick, 2),
		datedChange("channel.ch-1.dataset.scale", timelineKnick.Add(time.Hour), 0),
	)
	inline := domain.DatasetSource{Origin: domain.OriginFile, Ref: "d1", Resample: domain.ResampleHold, Anchor: domain.AnchorLoop}
	if got := index.effectiveDataset(domain.TimelineChannel, "ch-1", inline, timelineKnick.Add(-time.Second)); got.Scale != 0 {
		t.Errorf("before the knick the inline scale stands, got %v", got.Scale)
	}
	if got := index.effectiveDataset(domain.TimelineChannel, "ch-1", inline, timelineKnick); got.Scale != 2 {
		t.Errorf("from the knick on the declared scale applies, got %v", got.Scale)
	}
	if got := index.effectiveDataset(domain.TimelineChannel, "ch-1", inline, timelineKnick.Add(time.Hour)); got.Scale != 0 {
		t.Errorf("a change back to unscaled has to apply, got %v", got.Scale)
	}
	//the rest of the replay is not governed
	after := index.effectiveDataset(domain.TimelineChannel, "ch-1", inline, timelineKnick)
	if after.Ref != "d1" || after.Resample != domain.ResampleHold || after.Anchor != domain.AnchorLoop {
		t.Errorf("only the scale of a replay is governed, got %+v", after)
	}
}

// ---------------------------------------------------------------------------
// the remaining hook-ins, driven through their executors
// ---------------------------------------------------------------------------

// A formula reading a governed context key sees the declared value of the
// instant it is computed for, whatever the live state holds - which is what
// makes the read-only layer a layer rather than a rule nobody enforces.
func TestAFormulaReadsAGovernedContextKeyThroughTheLayer(t *testing.T) {
	const id = "env-timeline-formula"
	def := testEnvironment(id, formulaChannel(id, "price * 10", map[string]string{"price": "context.price"}))
	def.Context = map[string]interface{}{"price": 0.30}
	def.Timeline = []domain.DatedChange{datedChange("context.price", timelineKnick, 0.42)}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	gen := newGeneration(def, nil)
	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id}}
	env.seed(gen, timelineKnick.Add(-time.Hour))
	//a stale value in the live map, of the kind a flush of an older document
	//leaves behind: the formula must not see it
	env.state.Context["price"] = 99.0
	binding := gen.sensors[0]

	values := []float64{}
	send := func(value interface{}) { values = append(values, numericOrZero(value)) }
	rt.executeFormula(env, gen, binding, send, timelineKnick.Add(-time.Second))
	rt.executeFormula(env, gen, binding, send, timelineKnick)
	if len(values) != 2 {
		t.Fatalf("expected two evaluations, got %v", values)
	}
	//before the first change the layer does not hold, so the live value stands -
	//which is the escape a document keeps until its first dated change
	if values[0] != 990 {
		t.Errorf("before the first change the live value stands, got %v", values[0])
	}
	if values[1] != 4.2 {
		t.Errorf("from the knick on the declared value applies, got %v", values[1])
	}
}

// A schedule publishes the value its step has at this instant, resolved when the
// programme reaches the step rather than when the change was written.
func TestAScheduleChannelPublishesTheDeclaredValueOfItsStep(t *testing.T) {
	const id = "env-timeline-schedule"
	source := shortSchedule(domain.ScheduleState{Name: "run", DurationSeconds: 3600, Value: 9000})
	def := testEnvironment(id, scheduleChannel("ch-1", serviceRefOf(id), 60, source))
	def.Timeline = []domain.DatedChange{
		datedChange("channel.ch-1.schedule.states.run.value", timelineKnick, 5000),
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	gen := newGeneration(def, nil)
	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id}}
	env.seed(gen, timelineKnick.Add(-time.Hour))
	binding := gen.sensors[0]

	values := []float64{}
	send := func(value interface{}) { values = append(values, numericOrZero(value)) }
	rt.executeSchedule(env, gen, binding, send, timelineKnick.Add(-time.Minute))
	rt.executeSchedule(env, gen, binding, send, timelineKnick)
	if len(values) != 2 || values[0] != 9000 || values[1] != 5000 {
		t.Errorf("expected the step to publish 9000 and then 5000, got %v", values)
	}
	//and the name of the running step is untouched by the change of its value
	if env.state.Assets[testAssetId]["programm"] != "run" {
		t.Errorf("the running state has to keep its name, got %v", env.state.Assets[testAssetId]["programm"])
	}
}

// A context source follows its own dated change, written under the key it
// drives.
func TestAContextSourceTickFollowsItsDatedChange(t *testing.T) {
	const id = "env-timeline-source"
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 60, flatProfile(1, 0)))
	def.ContextSources = map[string]domain.Source{"outside": {
		Kind: domain.SourceProfile, IntervalSeconds: 300, Profile: &domain.ProfileSource{Base: 12},
	}}
	def.Timeline = []domain.DatedChange{
		datedChange("context_source.outside.profile.base", timelineKnick, 25),
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	gen := newGeneration(def, nil)
	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id}}
	env.seed(gen, timelineKnick.Add(-time.Hour))

	rt.tickContextSource(env, gen, "outside", def.ContextSources["outside"], timelineKnick.Add(-time.Second))
	if env.state.Context["outside"] != 12.0 {
		t.Errorf("before the knick the inline base drives the key, got %v", env.state.Context["outside"])
	}
	rt.tickContextSource(env, gen, "outside", def.ContextSources["outside"], timelineKnick)
	if env.state.Context["outside"] != 25.0 {
		t.Errorf("from the knick on the declared base drives the key, got %v", env.state.Context["outside"])
	}
}

// The same for the replaying half of a context source: only its scale is
// governed, and it is addressed under the key rather than under the series id
// the anchors are kept beneath.
func TestAReplayingContextSourceFollowsItsScaleChange(t *testing.T) {
	const id = "env-timeline-source-replay"
	def := testEnvironment(id, profileChannel("ch-1", serviceRefOf(id), 60, flatProfile(1, 0)))
	def.ContextSources = map[string]domain.Source{"sun": {
		Kind: domain.SourceDataset, IntervalSeconds: 300, Dataset: &domain.DatasetSource{
			Origin: domain.OriginFile, Ref: "d1", Resample: domain.ResampleHold, Anchor: domain.AnchorLoop},
	}}
	def.Timeline = []domain.DatedChange{
		datedChange("context_source.sun.dataset.scale", timelineKnick, 3),
	}
	rt := newRuntime(testConfig(time.Hour), newFakeEnvironments(def), newFakeStates(), nil, &fakePublisher{})
	//the anchor is created on the first tick, so both ticks below replay from the
	//same position and only the scale differs between them
	gen := newGeneration(def, map[string][]dataset.Point{contextSeriesId("sun"): parityPoints()})
	env := &environment{id: id, state: repo.RuntimeState{EnvironmentId: id}}
	env.seed(gen, timelineKnick.Add(-time.Hour))
	source := def.ContextSources["sun"]

	rt.tickContextSource(env, gen, "sun", source, timelineKnick.Add(-time.Second))
	unscaled := numericOrZero(env.state.Context["sun"])
	rt.tickContextSource(env, gen, "sun", source, timelineKnick)
	scaled := numericOrZero(env.state.Context["sun"])
	if unscaled == 0 {
		t.Fatal("the replay produced nothing, so the comparison proves nothing")
	}
	//a whole number times a power-of-two sample: the comparison is exact
	if scaled != 3*unscaled {
		t.Errorf("from the knick on the replay is scaled by three: %v and %v", unscaled, scaled)
	}
}

// A context source is addressed by the key it writes, not by a channel id, and
// the two namespaces do not reach into each other.
func TestAContextSourceProfileKnickIsAddressedByItsKey(t *testing.T) {
	index := indexOf(datedChange("context_source.outside.profile.base", timelineKnick, 25))
	inline := domain.ProfileSource{Base: 12}

	if got := index.effectiveProfile(domain.TimelineContextSource, "outside", inline, timelineKnick); got.Base != 25 {
		t.Errorf("the context source has to follow its own change, got %v", got.Base)
	}
	//a channel that happens to carry the same id is a different target
	if got := index.effectiveProfile(domain.TimelineChannel, "outside", inline, timelineKnick); got.Base != 12 {
		t.Errorf("a channel must not follow a context source's change, got %v", got.Base)
	}
}
