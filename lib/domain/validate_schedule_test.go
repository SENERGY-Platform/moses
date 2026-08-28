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
	"strings"
	"testing"
)

// scheduleEnvironment is the shape the source was built for: a machine that
// idles, sets itself up and then runs, with its air demand declared per state
// and a shift calendar in the context it could be gated on.
func scheduleEnvironment(mutate func(*Channel)) Environment {
	channel := Channel{
		Id: "ch-power", Name: "Wirkleistung", Direction: Sensor, IntervalSeconds: 10,
		Source: Source{Kind: SourceSchedule, Schedule: &ScheduleSource{
			StateKey: "programm",
			States: []ScheduleState{
				{Name: "idle", DurationSeconds: 600, Value: 400},
				{Name: "setup", DurationSeconds: 300, Value: 2000, DurationSpreadPercent: 20},
				{Name: "running", DurationSeconds: 1800, Value: 9000, SpreadPercent: 5,
					StateWrites: map[string]float64{"air_demand": 120}},
			},
		}},
	}
	if mutate != nil {
		mutate(&channel)
	}
	return Environment{
		Id: "e1", Name: "Werk", Type: IndustrialSite, Owner: "o",
		Context: map[string]interface{}{"shift": float64(0)},
		Zones: []Zone{{Id: "z1", Name: "Halle", Type: ZoneHall,
			Assets: []Asset{{Id: "a1", Name: "Fräse", Kind: AssetMachine,
				ExternalTypeId: "urn:infai:ses:device-type:x",
				Channels:       []Channel{channel}}}}},
	}
}

func expectScheduleProblem(t *testing.T, mutate func(*Channel), fragment string) {
	t.Helper()
	err := Validate(scheduleEnvironment(mutate))
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Errorf("expected a problem mentioning %q, got %v", fragment, err)
	}
}

func gated(key string, threshold float64) func(*Channel) {
	return func(c *Channel) {
		c.Source.Schedule.Gate = &ScheduleGate{ContextKey: key, Threshold: threshold}
	}
}

func TestValidateAcceptsAScheduleSource(t *testing.T) {
	if err := Validate(scheduleEnvironment(nil)); err != nil {
		t.Errorf("a declared machine programme has to be storable: %v", err)
	}
	if err := Validate(scheduleEnvironment(gated("shift", 0))); err != nil {
		t.Errorf("a schedule gated on a static context key has to be storable: %v", err)
	}
	if err := Validate(scheduleEnvironment(func(c *Channel) { c.Source.Schedule.RunOnce = true })); err != nil {
		t.Errorf("a single pass programme has to be storable: %v", err)
	}
}

// A gate may name a key that is driven rather than static: a shift calendar is
// exactly that, and it does not appear in context at all.
func TestValidateAcceptsAGateOnADrivenContextKey(t *testing.T) {
	env := scheduleEnvironment(gated("calendar", 0))
	env.ContextSources = map[string]Source{"calendar": {
		Kind: SourceProfile, IntervalSeconds: 300, Profile: &ProfileSource{Base: 1},
	}}
	if err := Validate(env); err != nil {
		t.Errorf("a gate on a context source key has to be storable: %v", err)
	}
}

func TestValidateRefusesBrokenSchedules(t *testing.T) {
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule = nil }, "must be set")
	expectScheduleProblem(t, func(c *Channel) { c.Source.IntervalSeconds = 5 }, "no own interval")
	expectScheduleProblem(t, func(c *Channel) { c.IntervalSeconds = 0 }, "must be a sensor with an interval")
	expectScheduleProblem(t, func(c *Channel) { c.Direction = Actuator; c.IntervalSeconds = 0 }, "must be a sensor with an interval")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States = nil }, "at least one state")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States = make([]ScheduleState, MaxScheduleStates+1)
	}, "at most 256 states")

	//per state
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].Name = "  " }, "must not be empty")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[1].Name = "idle" }, "duplicate state name")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].DurationSeconds = 0 }, "greater than zero")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].DurationSeconds = -1 }, "greater than zero")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[0].DurationSeconds = MaxScheduleDurationSeconds + 1
	}, "at most 31622400 seconds")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].DurationSpreadPercent = 100 }, "less than 100")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].DurationSpreadPercent = -1 }, "less than 100")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].SpreadPercent = -1 }, "must not be negative")

	//values that cannot be compared or stored
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].Value = math.NaN() }, "must be a finite number")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].Value = math.Inf(1) }, "must be a finite number")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[0].SpreadPercent = math.NaN()
	}, "must be a finite number")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[2].StateWrites["air_demand"] = math.Inf(-1)
	}, "must be a finite number")

	//keys
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.StateKey = " " }, "must not be empty")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.StateKey = "a.b" }, "must not contain")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.StateKey = "a$b" }, "must not contain")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[2].StateWrites = map[string]float64{"": 1}
	}, "must not be empty")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[2].StateWrites = map[string]float64{"air.demand": 1}
	}, "must not contain")

	//the gate
	expectScheduleProblem(t, gated("", 0), "must name the context key")
	expectScheduleProblem(t, gated("gibt-es-nicht", 0), "neither in context nor driven by a context source")
	//the message has to name the way out, because the key may well be written
	//at runtime by a script or by the state endpoint: the rule is that the
	//document says so, and declaring an initial 0 is the whole cost of it
	expectScheduleProblem(t, gated("gibt-es-nicht", 0), "declare it in context (an initial 0 is enough)")
	expectScheduleProblem(t, gated("shift", math.NaN()), "must be a finite number")

	//a second variant next to the schedule
	expectScheduleProblem(t, func(c *Channel) { c.Source.Profile = &ProfileSource{Base: 1} }, "only one source variant")
}

// Whitespace around a name or a key is neither trimmed nor harmless: the string
// is written into the asset state exactly as it stands, so " idle " is what a
// formula compares against while the editor, the docs and every reader see
// "idle". Two of them next to each other are two states nothing can tell apart,
// and one of them alone is a comparison that never matches.
func TestValidateRefusesWhitespaceAroundAScheduleNameOrKey(t *testing.T) {
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].Name = " idle" }, "must not begin or end with whitespace")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[0].Name = "idle\t" }, "must not begin or end with whitespace")
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.StateKey = "programm " }, "must not begin or end with whitespace")
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[2].StateWrites = map[string]float64{" air_demand": 1}
	}, "must not begin or end with whitespace")
	//the gate key is looked up in the context exactly as it stands, so this one
	//is a machine that waits for a calendar it can never read
	expectScheduleProblem(t, gated(" shift ", 0), "must not begin or end with whitespace")

	//and the pair the rule is really about: " idle " next to "idle" is refused
	//as a duplicate today, although the two are written distinguishably
	expectScheduleProblem(t, func(c *Channel) { c.Source.Schedule.States[1].Name = " idle " }, "must not begin or end with whitespace")

	//an inner space is a legitimate name and stays one
	if err := Validate(scheduleEnvironment(func(c *Channel) {
		c.Source.Schedule.States[0].Name = "warten auf material"
	})); err != nil {
		t.Errorf("a name with an inner space has to be storable: %v", err)
	}
}

// "off" is what a gated schedule writes while its gate is closed. A state of
// that name would make "the machine is standing still" and "the machine is in
// the state its author called off" the same string.
func TestValidateRefusesAGatedScheduleWithAnOffState(t *testing.T) {
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.Gate = &ScheduleGate{ContextKey: "shift"}
		c.Source.Schedule.States[0].Name = ScheduleClosedState
	}, "could not be told apart from the machine standing still")

	//without a gate nothing ever writes it, so the name is free
	if err := Validate(scheduleEnvironment(func(c *Channel) {
		c.Source.Schedule.States[0].Name = ScheduleClosedState
	})); err != nil {
		t.Errorf("a schedule without a gate may call a state %q: %v", ScheduleClosedState, err)
	}
}

func TestValidateRefusesAScheduleAsAContextSource(t *testing.T) {
	env := scheduleEnvironment(nil)
	env.ContextSources = map[string]Source{"k": {
		Kind: SourceSchedule, IntervalSeconds: 60,
		Schedule: &ScheduleSource{StateKey: "s", States: []ScheduleState{{Name: "a", DurationSeconds: 1}}},
	}}
	err := Validate(env)
	if err == nil || !strings.Contains(err.Error(), "not supported for context sources") {
		t.Errorf("a schedule has no asset to write its state into and must stay refused here, got %v", err)
	}
}

// twoChannelAsset puts a schedule next to another channel on one asset, which
// is where the asset state map is shared and a key can have two writers.
func twoChannelAsset(stateKey string, writes map[string]float64, neighbour Channel) Environment {
	env := scheduleEnvironment(func(c *Channel) {
		c.Source.Schedule.StateKey = stateKey
		c.Source.Schedule.States[2].StateWrites = writes
	})
	env.Zones[0].Assets[0].Channels = append(env.Zones[0].Assets[0].Channels, neighbour)
	return env
}

func TestValidateRefusesAScheduleWritingOverAChannelOfItsOwnAsset(t *testing.T) {
	counter := Channel{
		Id: "ch-meter", Name: "Zähler", Direction: Sensor, IntervalSeconds: 60,
		Source: Source{Kind: SourceProfile, Profile: &ProfileSource{Base: 10, Cumulative: true}},
	}
	expect := func(env Environment, fragment string) {
		t.Helper()
		err := Validate(env)
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("expected %q, got %v", fragment, err)
		}
	}
	//the state key is a channel id: a cumulative profile stores its reading
	//under exactly that key
	expect(twoChannelAsset("ch-meter", map[string]float64{"air_demand": 1}, counter),
		"which is the channel id of a channel of the same asset")
	//and so is a state write
	expect(twoChannelAsset("programm", map[string]float64{"ch-meter": 1}, counter),
		"which is the channel id of a channel of the same asset")
}

func TestValidateRefusesTwoSchedulesOfOneAssetSharingAKey(t *testing.T) {
	second := Channel{
		Id: "ch-second", Name: "Zweite", Direction: Sensor, IntervalSeconds: 10,
		Source: Source{Kind: SourceSchedule, Schedule: &ScheduleSource{
			StateKey: "programm-2",
			States: []ScheduleState{{Name: "a", DurationSeconds: 60, Value: 1,
				StateWrites: map[string]float64{"air_demand": 5}}},
		}},
	}
	err := Validate(twoChannelAsset("programm", map[string]float64{"air_demand": 120}, second))
	if err == nil || !strings.Contains(err.Error(), "already writes") {
		t.Errorf("two schedules of one asset must not both write air_demand, got %v", err)
	}

	//a key declared by several states of the SAME schedule is the union
	//semantics working as intended, not a collision
	if err := Validate(scheduleEnvironment(func(c *Channel) {
		c.Source.Schedule.States[0].StateWrites = map[string]float64{"air_demand": 0}
		c.Source.Schedule.States[1].StateWrites = map[string]float64{"air_demand": 30}
	})); err != nil {
		t.Errorf("one schedule declaring a key in several states has to pass: %v", err)
	}
}

func TestValidateRefusesAScheduleWritingOverItsOwnStateKey(t *testing.T) {
	expectScheduleProblem(t, func(c *Channel) {
		c.Source.Schedule.States[2].StateWrites = map[string]float64{"programm": 1}
	}, "would be overwritten by a number")
}
