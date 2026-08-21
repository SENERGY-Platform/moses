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

package migration

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/state"
)

// legacyWorld is a small but complete legacy world: an owner, one room, one
// device with a device type, one sensor service and a change routine on every
// level, so that a converted document is valid and still reports the expected
// findings.
func legacyWorld(id string, name string) *state.World {
	return &state.World{
		Id:     id,
		Name:   name,
		Owner:  "owner-1",
		States: map[string]interface{}{"outside": 12.5},
		ChangeRoutines: map[string]state.ChangeRoutine{
			"world-routine": {Id: "world-routine", Interval: 60, Code: "moses.world.state.set('outside', 1);"},
		},
		Rooms: map[string]*state.Room{
			"room-1": {
				Id:     "room-1",
				Name:   "Halle",
				States: map[string]interface{}{"humidity": 40.0},
				ChangeRoutines: map[string]state.ChangeRoutine{
					"room-routine": {Id: "room-routine", Interval: 30},
				},
				Devices: map[string]*state.Device{
					"device-1": {
						Id:             "device-1",
						Name:           "Kompressor",
						ExternalRef:    "urn:infai:ses:device:7283f08c",
						ExternalTypeId: "urn:infai:ses:device-type:dc5bf705",
						States:         map[string]interface{}{"kwh": 290508.57080252626},
						ChangeRoutines: map[string]state.ChangeRoutine{
							"device-routine": {Id: "device-routine", Interval: 15},
						},
						Services: map[string]state.Service{
							"service-1": {
								Id:             "service-1",
								Name:           "getTemperature",
								ExternalRef:    "urn:infai:ses:service:38657ee1",
								SensorInterval: 30,
								Code:           "moses.service.send(1);",
							},
						},
					},
				},
			},
		},
	}
}

func onlyPlan(t *testing.T, plans []WorldPlan) WorldPlan {
	t.Helper()
	if len(plans) != 1 {
		t.Fatalf("expected exactly one plan, got %d", len(plans))
	}
	return plans[0]
}

func planIds(plans []WorldPlan) []string {
	result := make([]string, 0, len(plans))
	for _, plan := range plans {
		result = append(result, plan.WorldId)
	}
	return result
}

func problemPaths(problems []domain.Problem) []string {
	result := make([]string, 0, len(problems))
	for _, problem := range problems {
		result = append(result, problem.Path)
	}
	return result
}

func TestPlanConvertsAWorldIntoAWritablePlan(t *testing.T) {
	plans := Plan(map[string]*state.World{"world-1": legacyWorld("world-1", "Werk")}, domain.IndustrialSite, nil)
	plan := onlyPlan(t, plans)
	if plan.WorldId != "world-1" || plan.WorldName != "Werk" {
		t.Errorf("unexpected identity %v / %v", plan.WorldId, plan.WorldName)
	}
	if plan.Environment.Id != "world-1" {
		t.Errorf("the environment id has to be the legacy world id, got %v", plan.Environment.Id)
	}
	if plan.Environment.Owner != "owner-1" {
		t.Errorf("the owner was not carried over, got %q", plan.Environment.Owner)
	}
	if plan.Environment.Type != domain.IndustrialSite {
		t.Errorf("unexpected environment type %v", plan.Environment.Type)
	}
	if plan.Err != nil {
		t.Errorf("the plan is not writable: %v", plan.Err)
	}
	if plan.Skip || plan.SkipReason != "" {
		t.Errorf("nothing exists yet, so nothing may be skipped: %v", plan.SkipReason)
	}
	if !plan.Writable() || plan.Blocked() {
		t.Errorf("writable=%v blocked=%v", plan.Writable(), plan.Blocked())
	}
	zones, assets, channels := plan.Counts()
	if zones != 1 || assets != 1 || channels != 1 {
		t.Errorf("expected 1/1/1 zones/assets/channels, got %d/%d/%d", zones, assets, channels)
	}
	if len(plan.UnmappedRoutines()) != 3 {
		t.Errorf("expected the world, room and device routine to be reported, got %v", problemPaths(plan.UnmappedRoutines()))
	}
	if len(plan.OtherProblems()) != 0 {
		t.Errorf("unexpected findings: %v", plan.OtherProblems())
	}
}

func TestPlanSkipsAWorldThatAlreadyExistsInTheEnvironmentStore(t *testing.T) {
	plans := Plan(map[string]*state.World{"world-1": legacyWorld("world-1", "Werk")}, domain.IndustrialSite, map[string]bool{"world-1": true})
	plan := onlyPlan(t, plans)
	if !plan.Skip {
		t.Fatal("an existing environment id has to be skipped")
	}
	if plan.Writable() {
		t.Error("a skipped plan must not be written")
	}
	if plan.Blocked() {
		t.Error("a skipped plan is not a failure")
	}
	if !strings.Contains(plan.SkipReason, "world-1") || !strings.Contains(plan.SkipReason, "already exists") {
		t.Errorf("the skip reason has to name the id and the reason, got %q", plan.SkipReason)
	}
	// the conversion result is still reported, so that a dry run shows what
	// would have been written
	if plan.Environment.Id != "world-1" || len(plan.UnmappedRoutines()) != 3 {
		t.Errorf("a skipped plan still has to carry its conversion, got %v with %d routines", plan.Environment.Id, len(plan.UnmappedRoutines()))
	}
}

// the ids are compared after trimming, because the conversion trims: a legacy id
// with a stray space would otherwise be looked up as " abc " and stored as "abc",
// and every run would insert another copy.
func TestPlanSkipDetectionUsesTheTrimmedId(t *testing.T) {
	plans := Plan(map[string]*state.World{" world-1 ": legacyWorld(" world-1 ", "Werk")}, domain.IndustrialSite, map[string]bool{"world-1": true})
	plan := onlyPlan(t, plans)
	if plan.Environment.Id != "world-1" {
		t.Fatalf("the conversion trims the id, got %q", plan.Environment.Id)
	}
	if !plan.Skip {
		t.Error("the world exists under the trimmed id and has to be skipped")
	}
}

func TestPlanPassesAValidationErrorThrough(t *testing.T) {
	world := legacyWorld("world-1", "Werk")
	//a device without a device type is the case the conversion cannot repair
	world.Rooms["room-1"].Devices["device-1"].ExternalTypeId = ""
	plan := onlyPlan(t, Plan(map[string]*state.World{"world-1": world}, domain.IndustrialSite, nil))
	if plan.Err == nil {
		t.Fatal("a device without a device type has to fail validation")
	}
	validation := &domain.ValidationError{}
	if !errors.As(plan.Err, &validation) {
		t.Fatalf("expected a *domain.ValidationError, got %T: %v", plan.Err, plan.Err)
	}
	if len(validation.Problems) != 1 || !strings.Contains(validation.Problems[0].Path, "external_type_id") {
		t.Errorf("unexpected validation problems: %v", validation.Problems)
	}
	if plan.Writable() || !plan.Blocked() {
		t.Errorf("an invalid document must not be written and has to be reported: writable=%v blocked=%v", plan.Writable(), plan.Blocked())
	}
	//the environment is still filled, so the report can show what was rejected
	if plan.Environment.Id != "world-1" {
		t.Error("the rejected conversion still has to be reported")
	}
}

// a validation failure on a world that is skipped anyway is information, not a
// failure: nothing is written either way and the stored environment is what runs.
func TestPlanDoesNotTurnASkippedWorldIntoAFailure(t *testing.T) {
	world := legacyWorld("world-1", "Werk")
	world.Rooms["room-1"].Devices["device-1"].ExternalTypeId = ""
	plan := onlyPlan(t, Plan(map[string]*state.World{"world-1": world}, domain.IndustrialSite, map[string]bool{"world-1": true}))
	if !plan.Skip {
		t.Fatal("the world exists and has to be skipped")
	}
	if plan.Blocked() {
		t.Error("a skipped world must not count as blocked")
	}
	if plan.Err == nil {
		t.Error("the validation result is still reported")
	}
}

func TestPlanOrdersByWorldNameThenId(t *testing.T) {
	worlds := map[string]*state.World{
		"c": legacyWorld("c", "Beta"),
		"a": legacyWorld("a", "Alpha"),
		"b": legacyWorld("b", "Beta"),
		"d": legacyWorld("d", "alpha"),
	}
	//"Alpha" < "Beta" < "alpha" in byte order, and the two "Beta" worlds are
	//ordered by their id
	want := []string{"a", "b", "c", "d"}
	if got := planIds(Plan(worlds, domain.IndustrialSite, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("expected the order %v, got %v", want, got)
	}
}

// map iteration order is randomised per range, so a single pass proves nothing.
func TestPlanOrderIsStableAcrossRuns(t *testing.T) {
	worlds := map[string]*state.World{}
	for _, id := range []string{"e", "d", "c", "b", "a"} {
		//deliberately all named the same, so that only the id breaks the tie
		worlds[id] = legacyWorld(id, "same name")
	}
	want := []string{"a", "b", "c", "d", "e"}
	for run := 0; run < 50; run++ {
		if got := planIds(Plan(worlds, domain.IndustrialSite, nil)); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d produced %v, expected %v", run, got, want)
		}
	}
}

// a world without an id would get a freshly generated one from the conversion,
// which differs on every run: a second run could not recognise it and would
// insert a second copy. it is blocked instead.
func TestPlanBlocksAWorldWithoutAnId(t *testing.T) {
	plan := onlyPlan(t, Plan(map[string]*state.World{"": legacyWorld("", "Werk")}, domain.IndustrialSite, nil))
	if !errors.Is(plan.Err, ErrNoLegacyId) {
		t.Fatalf("expected ErrNoLegacyId, got %v", plan.Err)
	}
	if plan.Writable() || !plan.Blocked() {
		t.Error("a world without an id must not be written")
	}
	if plan.WorldName != "Werk" {
		t.Errorf("the world still has to be identifiable in the report, got %q", plan.WorldName)
	}
}

func TestPlanBlocksAWorldWhoseIdIsOnlyWhitespace(t *testing.T) {
	plan := onlyPlan(t, Plan(map[string]*state.World{"   ": legacyWorld("   ", "Werk")}, domain.IndustrialSite, nil))
	if !errors.Is(plan.Err, ErrNoLegacyId) {
		t.Fatalf("expected ErrNoLegacyId, got %v", plan.Err)
	}
}

func TestPlanBlocksANilWorldWithoutLosingTheRest(t *testing.T) {
	plans := Plan(map[string]*state.World{
		"broken": nil,
		"ok":     legacyWorld("ok", "Zulu"),
	}, domain.IndustrialSite, nil)
	if len(plans) != 2 {
		t.Fatalf("expected two plans, got %d", len(plans))
	}
	//the nil world sorts first, because its name is empty
	if !errors.Is(plans[0].Err, ErrNilWorld) {
		t.Errorf("expected ErrNilWorld, got %v", plans[0].Err)
	}
	if plans[0].WorldId != "broken" {
		t.Errorf("the key has to identify the broken entry, got %q", plans[0].WorldId)
	}
	if !plans[1].Writable() {
		t.Errorf("the intact world has to stay writable: %v", plans[1].Err)
	}
}

// two worlds converting into the same environment id would mean the second write
// replaces the first. the map key normally prevents this, but the id in the
// document is what the conversion uses, and a map that was not built by
// LoadWorlds can disagree with its keys.
func TestPlanBlocksTheSecondWorldWithTheSameEnvironmentId(t *testing.T) {
	plans := Plan(map[string]*state.World{
		"key-a": legacyWorld("same-id", "Alpha"),
		"key-b": legacyWorld("same-id", "Beta"),
	}, domain.IndustrialSite, nil)
	if len(plans) != 2 {
		t.Fatalf("expected two plans, got %d", len(plans))
	}
	if !plans[0].Writable() {
		t.Errorf("the first world stays writable: %v", plans[0].Err)
	}
	if !errors.Is(plans[1].Err, ErrDuplicateEnvironmentId) {
		t.Fatalf("expected ErrDuplicateEnvironmentId, got %v", plans[1].Err)
	}
	if !strings.Contains(plans[1].Err.Error(), "same-id") {
		t.Errorf("the error has to name the colliding id, got %v", plans[1].Err)
	}
}

// state.World.Owner is bson only (json:"-"), so a world read from a json export
// carries no owner while the same world read from mongo does. an environment
// without an owner is in nobody's list, so it has to be reported.
func TestPlanReportsAWorldWithoutAnOwner(t *testing.T) {
	world := legacyWorld("world-1", "Werk")
	world.Owner = ""
	plan := onlyPlan(t, Plan(map[string]*state.World{"world-1": world}, domain.IndustrialSite, nil))
	found := false
	for _, problem := range plan.OtherProblems() {
		if problem.Path == "owner" {
			found = true
			if !strings.Contains(problem.Message, "json export") {
				t.Errorf("the message has to name the usual cause, got %q", problem.Message)
			}
		}
	}
	if !found {
		t.Errorf("expected a problem for the missing owner, got %v", problemPaths(plan.Problems))
	}
	//missing owner does not block: Validate does not require one and refusing the
	//migration over it would be worse than reporting it
	if !plan.Writable() {
		t.Errorf("a missing owner must not block the migration: %v", plan.Err)
	}
}

func TestPlanReportsAnUnknownEnvironmentTypeForEveryWorld(t *testing.T) {
	plans := Plan(map[string]*state.World{
		"a": legacyWorld("a", "Alpha"),
		"b": legacyWorld("b", "Beta"),
	}, domain.EnvironmentType("space_station"), nil)
	if len(plans) != 2 {
		t.Fatalf("expected two plans, got %d", len(plans))
	}
	for _, plan := range plans {
		if plan.Err == nil || !strings.Contains(plan.Err.Error(), "space_station") {
			t.Errorf("expected the unknown type to be reported, got %v", plan.Err)
		}
		if plan.Writable() {
			t.Error("nothing may be written for an unknown environment type")
		}
	}
}

func TestPlanReturnsAnEmptySliceForNoWorlds(t *testing.T) {
	plans := Plan(nil, domain.IndustrialSite, nil)
	if plans == nil || len(plans) != 0 {
		t.Errorf("expected an empty slice, got %v", plans)
	}
}

// the plan is printed and possibly written after the legacy store was read; it
// must not alias the legacy state maps, or a running legacy simulation could
// change what is being written.
func TestPlanDoesNotShareStateMapsWithTheLegacyWorld(t *testing.T) {
	world := legacyWorld("world-1", "Werk")
	plan := onlyPlan(t, Plan(map[string]*state.World{"world-1": world}, domain.IndustrialSite, nil))
	world.States["outside"] = 99.0
	world.Rooms["room-1"].States["humidity"] = 99.0
	world.Rooms["room-1"].Devices["device-1"].States["kwh"] = 99.0
	if plan.Environment.Context["outside"] != 12.5 {
		t.Errorf("the context is shared with the legacy world: %v", plan.Environment.Context["outside"])
	}
	if plan.Environment.Zones[0].InitialStates["humidity"] != 40.0 {
		t.Errorf("the zone states are shared with the legacy world: %v", plan.Environment.Zones[0].InitialStates)
	}
	if plan.Environment.Zones[0].Assets[0].InitialStates["kwh"] != 290508.57080252626 {
		t.Errorf("the asset states are shared with the legacy world: %v", plan.Environment.Zones[0].Assets[0].InitialStates)
	}
}

func TestUnmappedRoutinesAndOtherProblemsSplitTheFindings(t *testing.T) {
	plan := WorldPlan{Problems: []domain.Problem{
		{Path: "legacy:change_routines[a]", Message: "world routine"},
		{Path: "owner", Message: "no owner"},
		{Path: "legacy:rooms[r].devices[d].change_routines[b]", Message: "device routine"},
		{Path: "zones[0].assets[0].external_type_id", Message: "no device type"},
	}}
	if got := problemPaths(plan.UnmappedRoutines()); !reflect.DeepEqual(got, []string{"legacy:change_routines[a]", "legacy:rooms[r].devices[d].change_routines[b]"}) {
		t.Errorf("unexpected routines: %v", got)
	}
	if got := problemPaths(plan.OtherProblems()); !reflect.DeepEqual(got, []string{"owner", "zones[0].assets[0].external_type_id"}) {
		t.Errorf("unexpected other problems: %v", got)
	}
}

func TestCountsWalksNestedZones(t *testing.T) {
	plan := WorldPlan{Environment: domain.Environment{Zones: []domain.Zone{
		{
			Assets: []domain.Asset{{Channels: []domain.Channel{{}, {}}}},
			Zones: []domain.Zone{
				{Assets: []domain.Asset{{Channels: []domain.Channel{{}}}, {}}},
				{},
			},
		},
		{},
	}}}
	zones, assets, channels := plan.Counts()
	if zones != 4 || assets != 3 || channels != 3 {
		t.Errorf("expected 4/3/3, got %d/%d/%d", zones, assets, channels)
	}
}

func TestValidateEnvironmentTypeAcceptsEveryTypeTheModelKnows(t *testing.T) {
	for _, envType := range []domain.EnvironmentType{
		domain.IndustrialSite, domain.OfficeBuilding, domain.ApartmentBuilding, domain.SingleFamilyHome, domain.Apartment,
	} {
		if err := ValidateEnvironmentType(envType); err != nil {
			t.Errorf("%v was rejected: %v", envType, err)
		}
	}
}

func TestValidateEnvironmentTypeRejectsAnUnknownTypeAndNamesTheAlternatives(t *testing.T) {
	err := ValidateEnvironmentType(domain.EnvironmentType("space_station"))
	if err == nil {
		t.Fatal("an unknown type has to be rejected")
	}
	if !strings.Contains(err.Error(), "space_station") || !strings.Contains(err.Error(), string(domain.IndustrialSite)) {
		t.Errorf("the error has to name the rejected value and the accepted ones, got %v", err)
	}
	if err := ValidateEnvironmentType(""); err == nil {
		t.Error("an empty type has to be rejected")
	}
}

func TestCandidateIdsAreSortedDeduplicatedAndTrimmed(t *testing.T) {
	worlds := map[string]*state.World{
		"b":       legacyWorld("b", "Beta"),
		"a":       legacyWorld("a", "Alpha"),
		"dup":     legacyWorld("a", "Alpha again"),
		"spaced":  legacyWorld(" c ", "Gamma"),
		"no-id":   legacyWorld("", "no id"),
		"nil-one": nil,
	}
	want := []string{"a", "b", "c"}
	if got := CandidateIds(worlds); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
}

// the caller asks the environment store for CandidateIds and Plan compares
// against the answer: if the two derived the id differently, the skip detection
// would silently stop working.
func TestCandidateIdsMatchTheEnvironmentIdsThePlanProduces(t *testing.T) {
	worlds := map[string]*state.World{
		"a":      legacyWorld("a", "Alpha"),
		"b":      legacyWorld("b", "Beta"),
		"spaced": legacyWorld(" c ", "Gamma"),
	}
	candidates := map[string]bool{}
	for _, id := range CandidateIds(worlds) {
		candidates[id] = true
	}
	for _, plan := range Plan(worlds, domain.IndustrialSite, nil) {
		if !candidates[plan.Environment.Id] {
			t.Errorf("the plan writes %v, which was never asked about: %v", plan.Environment.Id, CandidateIds(worlds))
		}
	}
}

// industryWorld is the real production export, loaded the way lib/domain's own
// test loads it. The fixture is shared rather than copied: it is the document the
// conversion is golden tested against, and a second copy would drift.
func industryWorld(t *testing.T) *state.World {
	t.Helper()
	content, err := os.ReadFile("../domain/testdata/industry-world.json")
	if err != nil {
		t.Fatal(err)
	}
	world := state.World{}
	err = json.Unmarshal(content, &world)
	if err != nil {
		t.Fatal(err)
	}
	//World.Owner is json:"-" - it comes from the token, not from the document -
	//so the exported owner has to be applied by hand, exactly as the migration
	//gets it from mongo
	world.Owner = "aae7e87b-63a2-477f-afb4-caa0db84e3fa"
	return &world
}

func industryWorlds(t *testing.T) map[string]*state.World {
	t.Helper()
	world := industryWorld(t)
	return map[string]*state.World{world.Id: world}
}

func TestPlanConvertsTheIndustryExportIntoOneWritablePlan(t *testing.T) {
	plans := Plan(industryWorlds(t), domain.IndustrialSite, nil)
	plan := onlyPlan(t, plans)
	if plan.WorldName != "Industry" || plan.WorldId != "4d273cd0-838f-4f84-9974-e56d18245255" {
		t.Errorf("unexpected identity %q / %q", plan.WorldName, plan.WorldId)
	}
	if plan.Err != nil {
		t.Errorf("the production world does not convert into a valid environment: %v", plan.Err)
	}
	if plan.Skip {
		t.Error("nothing was migrated yet, so nothing may be skipped")
	}
	if !plan.Writable() {
		t.Error("the production world has to be writable")
	}
	zones, assets, channels := plan.Counts()
	if zones != 1 || assets != 3 || channels != 24 {
		t.Errorf("expected 1/3/24 zones/assets/channels, got %d/%d/%d", zones, assets, channels)
	}
	if plan.Environment.Owner != "aae7e87b-63a2-477f-afb4-caa0db84e3fa" {
		t.Errorf("the owner was not carried over, got %q", plan.Environment.Owner)
	}
}

func TestPlanReportsEveryUnmappedChangeRoutineOfTheIndustryExport(t *testing.T) {
	plan := onlyPlan(t, Plan(industryWorlds(t), domain.IndustrialSite, nil))
	if got := len(plan.UnmappedRoutines()); got != 21 {
		t.Errorf("expected 21 unmapped change routines, got %d: %v", got, problemPaths(plan.UnmappedRoutines()))
	}
	//the owner is set on the fixture, so nothing else is expected to be found
	if got := plan.OtherProblems(); len(got) != 0 {
		t.Errorf("unexpected findings besides the change routines: %v", got)
	}
}

func TestPlanSkipsTheIndustryExportOnASecondRun(t *testing.T) {
	worlds := industryWorlds(t)
	first := onlyPlan(t, Plan(worlds, domain.IndustrialSite, nil))
	if !first.Writable() {
		t.Fatalf("the first run has to write: %v", first.Err)
	}
	//the second run sees the id the first one wrote
	second := onlyPlan(t, Plan(worlds, domain.IndustrialSite, map[string]bool{first.Environment.Id: true}))
	if !second.Skip {
		t.Fatal("the second run has to skip the already migrated world")
	}
	if second.Writable() {
		t.Error("the second run must not write")
	}
	if second.Blocked() {
		t.Error("a skip is not a failure")
	}
	if !strings.Contains(second.SkipReason, first.Environment.Id) {
		t.Errorf("the skip reason has to name the id, got %q", second.SkipReason)
	}
}

// converting the same world twice has to produce the same document, otherwise a
// re-run would look like a change and the seed would move under the simulation.
func TestPlanIsRepeatableForTheIndustryExport(t *testing.T) {
	first := onlyPlan(t, Plan(industryWorlds(t), domain.IndustrialSite, nil))
	second := onlyPlan(t, Plan(industryWorlds(t), domain.IndustrialSite, nil))
	if !reflect.DeepEqual(first.Environment, second.Environment) {
		t.Error("two conversions of the same world produced different documents")
	}
}
