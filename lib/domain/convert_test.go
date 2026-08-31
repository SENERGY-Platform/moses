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
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/google/uuid"
)

// legacyWorld is one room with one device with one service, with every id set.
// The single-purpose tests below start from this and change one thing.
func legacyWorld() state.World {
	return state.World{
		Id:     "world-1",
		Owner:  "owner-1",
		Name:   "Industry",
		States: map[string]interface{}{"temperature": 21.5},
		Rooms: map[string]*state.Room{
			"room-key": {
				Id:     "room-1",
				Name:   "Fabric",
				States: map[string]interface{}{"humidity": 40.0},
				Devices: map[string]*state.Device{
					"device-key": {
						Id:             "device-1",
						Name:           "Compressor",
						ExternalTypeId: "urn:infai:ses:device-type:dc5bf705",
						ExternalRef:    "urn:infai:ses:device:7283f08c",
						States:         map[string]interface{}{"kwh": 290508.57080252626},
						Services: map[string]state.Service{
							"service-key": {
								Id:             "service-1",
								Name:           "getTemperatureService",
								ExternalRef:    "urn:infai:ses:service:38657ee1",
								SensorInterval: 30,
								Code:           "moses.service.send(moses.device.state.get(\"celsius\"));",
							},
						},
					},
				},
			},
		},
	}
}

func convert(t *testing.T, world state.World) (Environment, []Problem) {
	t.Helper()
	env, problems, err := FromLegacyWorld(world, IndustrialSite)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return env, problems
}

// firstChannel is the only channel of the only asset of the only zone.
func firstChannel(t *testing.T, env Environment) Channel {
	t.Helper()
	if len(env.Zones) != 1 || len(env.Zones[0].Assets) != 1 || len(env.Zones[0].Assets[0].Channels) != 1 {
		t.Fatalf("expected exactly one zone with one asset with one channel, got %#v", env.Zones)
	}
	return env.Zones[0].Assets[0].Channels[0]
}

func problemMessages(problems []Problem) string {
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.String())
	}
	return strings.Join(parts, "\n")
}

func assertProblemMentions(t *testing.T, problems []Problem, needle string) {
	t.Helper()
	for _, problem := range problems {
		if strings.Contains(problem.Path, needle) || strings.Contains(problem.Message, needle) {
			return
		}
	}
	t.Errorf("expected a problem mentioning %q, got:\n%v", needle, problemMessages(problems))
}

func assertNoProblems(t *testing.T, problems []Problem) {
	t.Helper()
	if len(problems) != 0 {
		t.Errorf("expected no problems, got:\n%v", problemMessages(problems))
	}
}

func TestFromLegacyWorldCarriesTheDevicePlatformReferencesVerbatim(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	asset := env.Zones[0].Assets[0]
	if asset.ExternalRef != "urn:infai:ses:device:7283f08c" {
		t.Errorf("external_ref was rewritten: %v", asset.ExternalRef)
	}
	if asset.ExternalTypeId != "urn:infai:ses:device-type:dc5bf705" {
		t.Errorf("external_type_id was rewritten: %v", asset.ExternalTypeId)
	}
}

func TestFromLegacyWorldCarriesTheServicePlatformReferenceVerbatim(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	if firstChannel(t, env).ExternalRef != "urn:infai:ses:service:38657ee1" {
		t.Errorf("external_ref was rewritten: %v", firstChannel(t, env).ExternalRef)
	}
}

func TestFromLegacyWorldDerivesTheDirectionFromTheSensorInterval(t *testing.T) {
	cases := []struct {
		name          string
		interval      int64
		wantDirection Direction
		wantInterval  int64
	}{
		{name: "a positive interval makes a sensor", interval: 30, wantDirection: Sensor, wantInterval: 30},
		{name: "an hourly interval makes a sensor", interval: 3600, wantDirection: Sensor, wantInterval: 3600},
		{name: "no interval makes an actuator", interval: 0, wantDirection: Actuator, wantInterval: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			world := legacyWorld()
			service := world.Rooms["room-key"].Devices["device-key"].Services["service-key"]
			service.SensorInterval = testCase.interval
			world.Rooms["room-key"].Devices["device-key"].Services["service-key"] = service

			channel := firstChannel(t, mustConvert(t, world))
			if channel.Direction != testCase.wantDirection || channel.IntervalSeconds != testCase.wantInterval {
				t.Errorf("interval %d became direction %q with interval %d, want %q with %d",
					testCase.interval, channel.Direction, channel.IntervalSeconds, testCase.wantDirection, testCase.wantInterval)
			}
		})
	}
}

func mustConvert(t *testing.T, world state.World) Environment {
	t.Helper()
	env, _ := convert(t, world)
	return env
}

func TestFromLegacyWorldResetsANegativeSensorIntervalToAnActuator(t *testing.T) {
	world := legacyWorld()
	service := world.Rooms["room-key"].Devices["device-key"].Services["service-key"]
	service.SensorInterval = -5
	world.Rooms["room-key"].Devices["device-key"].Services["service-key"] = service

	env, problems := convert(t, world)
	channel := firstChannel(t, env)
	if channel.Direction != Actuator || channel.IntervalSeconds != 0 {
		t.Errorf("a negative interval became direction %q with interval %d, want %q with 0", channel.Direction, channel.IntervalSeconds, Actuator)
	}
	assertProblemMentions(t, problems, "negative")
	if err := Validate(env); err != nil {
		t.Errorf("the result of a negative interval must still be valid: %v", err)
	}
}

func TestFromLegacyWorldCarriesTheServiceCodeIntoAScriptSource(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	channel := firstChannel(t, env)
	if channel.Source.Kind != SourceScript {
		t.Fatalf("expected source kind %q, got %q", SourceScript, channel.Source.Kind)
	}
	if channel.Source.Script == nil || channel.Source.Script.Code != "moses.service.send(moses.device.state.get(\"celsius\"));" {
		t.Errorf("the service code was not carried over: %#v", channel.Source.Script)
	}
}

func TestFromLegacyWorldLeavesTheCharacteristicToTheDeviceRepository(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	channel := firstChannel(t, env)
	if channel.CharacteristicId != "" || channel.Unit != "" {
		t.Errorf("characteristic and unit must be resolved elsewhere, got %q / %q", channel.CharacteristicId, channel.Unit)
	}
}

// A world or zone routine is not migrated by decision: it evolves a state that
// no channel can publish, because channels belong to assets. Zone measurements
// are modelled as their own assets instead of being translated.
func TestFromLegacyWorldDoesNotMigrateAWorldChangeRoutine(t *testing.T) {
	world := legacyWorld()
	world.ChangeRoutines = map[string]state.ChangeRoutine{
		"routine-key": {Id: "routine-world", Interval: 15, Code: "moses.world.state.set(\"temperature\", 1);"},
	}
	_, problems := convert(t, world)
	assertProblemMentions(t, problems, "routine-world")
	assertProblemMentions(t, problems, "15s")
	assertProblemMentions(t, problems, "deliberately not migrated")
	assertProblemMentions(t, problems, "legacy:change_routines_not_migrated[routine-key]")
}

func TestFromLegacyWorldDoesNotMigrateARoomChangeRoutine(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].ChangeRoutines = map[string]state.ChangeRoutine{
		"routine-key": {Id: "routine-room", Interval: 20, Code: "moses.room.state.set(\"humidity\", 1);"},
	}
	_, problems := convert(t, world)
	assertProblemMentions(t, problems, "routine-room")
	assertProblemMentions(t, problems, "deliberately not migrated")
	assertProblemMentions(t, problems, "legacy:rooms[room-key].change_routines_not_migrated[routine-key]")
}

// This is what the source interval is for: the routine keeps its own 5 seconds
// while the channel keeps publishing every 30, as the legacy pair did.
func TestFromLegacyWorldAttachesADeviceChangeRoutineToTheChannelReadingItsState(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices["device-key"].ChangeRoutines = map[string]state.ChangeRoutine{
		"routine-key": {Id: "routine-device", Interval: 5, Code: "moses.device.state.set(\"celsius\", 1);"},
	}
	env, problems := convert(t, world)
	assertNoProblems(t, problems)

	channel := firstChannel(t, env)
	if channel.Source.IntervalSeconds != 5 {
		t.Errorf("expected the routine interval 5 on the source, got %d", channel.Source.IntervalSeconds)
	}
	if channel.IntervalSeconds != 30 {
		t.Errorf("expected the publish interval to stay at 30, got %d", channel.IntervalSeconds)
	}
	if !strings.Contains(channel.Source.Script.Code, "moses.device.state.set(\"celsius\", 1);") {
		t.Errorf("the routine script was not carried into the source: %q", channel.Source.Script.Code)
	}
	if !strings.Contains(channel.Source.Script.Code, "moses.service.send") {
		t.Errorf("the legacy service script was lost: %q", channel.Source.Script.Code)
	}
}

// A routine writing two states is attached once. Both values come out of one
// draw, so running the physics twice would decouple them.
func TestFromLegacyWorldAttachesATwoStateRoutineToOneChannelOnly(t *testing.T) {
	world := legacyWorld()
	device := world.Rooms["room-key"].Devices["device-key"]
	device.Services["second"] = state.Service{
		Id: "service-2", Name: "getPressureService", ExternalRef: "urn:infai:ses:service:2",
		SensorInterval: 30, Code: "moses.service.send(moses.device.state.get(\"bar\"));",
	}
	device.ChangeRoutines = map[string]state.ChangeRoutine{
		"routine-key": {Id: "routine-two", Interval: 5,
			Code: "moses.device.state.set(\"bar\", 1); moses.device.state.set(\"celsius\", 2);"},
	}
	env, problems := convert(t, world)
	assertNoProblems(t, problems)

	withSource, plain := 0, 0
	for _, channel := range env.Zones[0].Assets[0].Channels {
		if channel.Source.IntervalSeconds == 5 {
			withSource++
		} else if channel.Source.IntervalSeconds == 0 {
			plain++
		}
	}
	if withSource != 1 || plain != 1 {
		t.Errorf("expected one channel to carry the routine and one to stay a reader, got %d and %d", withSource, plain)
	}
}

func TestFromLegacyWorldReportsADeviceChangeRoutineNoChannelReads(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices["device-key"].ChangeRoutines = map[string]state.ChangeRoutine{
		"routine-key": {Id: "routine-device", Interval: 5, Code: "moses.device.state.set(\"unread\", 1);"},
	}
	_, problems := convert(t, world)
	assertProblemMentions(t, problems, "routine-device")
	assertProblemMentions(t, problems, "writes no device state that a channel of this asset reads")
	assertProblemMentions(t, problems, "legacy:rooms[room-key].devices[device-key].change_routines[routine-key]")
}

func TestFromLegacyWorldKeepsLegacyIdsThatExist(t *testing.T) {
	env, problems := convert(t, legacyWorld())
	assertNoProblems(t, problems)
	got := []string{env.Id, env.Zones[0].Id, env.Zones[0].Assets[0].Id, firstChannel(t, env).Id}
	want := []string{"world-1", "room-1", "device-1", "service-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ids were not preserved: got %v, want %v", got, want)
	}
}

func TestFromLegacyWorldFallsBackToTheLegacyMapKeyAsId(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Id = ""
	world.Rooms["room-key"].Devices["device-key"].Id = ""
	service := world.Rooms["room-key"].Devices["device-key"].Services["service-key"]
	service.Id = ""
	world.Rooms["room-key"].Devices["device-key"].Services["service-key"] = service

	env, _ := convert(t, world)
	got := []string{env.Zones[0].Id, env.Zones[0].Assets[0].Id, firstChannel(t, env).Id}
	want := []string{"room-key", "device-key", "service-key"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the legacy map key was not used as id: got %v, want %v", got, want)
	}
}

func TestFromLegacyWorldGeneratesAnIdWhenTheLegacyDocumentHasNone(t *testing.T) {
	world := legacyWorld()
	world.Id = ""
	env, problems := convert(t, world)
	if _, err := uuid.Parse(env.Id); err != nil {
		t.Errorf("expected a generated uuid, got %q: %v", env.Id, err)
	}
	assertProblemMentions(t, problems, "no id")
}

func TestFromLegacyWorldResolvesADuplicateLegacyId(t *testing.T) {
	world := legacyWorld()
	//two devices in the same room claiming the same id: the second one has to be
	//given a different id, or the document is rejected as ambiguous
	world.Rooms["room-key"].Devices["second-device-key"] = &state.Device{
		Id:             "device-1",
		Name:           "Compressor 2",
		ExternalTypeId: "urn:infai:ses:device-type:dc5bf705",
		ExternalRef:    "urn:infai:ses:device:second",
	}
	env, problems := convert(t, world)
	assets := env.Zones[0].Assets
	if len(assets) != 2 {
		t.Fatalf("expected two assets, got %d", len(assets))
	}
	if assets[0].Id == assets[1].Id {
		t.Errorf("both assets kept the id %v", assets[0].Id)
	}
	assertProblemMentions(t, problems, "already used")
	if err := Validate(env); err != nil {
		t.Errorf("a resolved duplicate must leave a valid environment: %v", err)
	}
}

func TestFromLegacyWorldProducesAValidEnvironment(t *testing.T) {
	env, problems := convert(t, legacyWorld())
	assertNoProblems(t, problems)
	if err := Validate(env); err != nil {
		t.Errorf("the converted environment is not valid: %v", err)
	}
}

func TestFromLegacyWorldCarriesOwnerNameAndStates(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	if env.Owner != "owner-1" {
		t.Errorf("owner was not carried over: %q", env.Owner)
	}
	if env.Name != "Industry" {
		t.Errorf("name was not carried over: %q", env.Name)
	}
	if !reflect.DeepEqual(env.Context, map[string]interface{}{"temperature": 21.5}) {
		t.Errorf("world states did not become the context: %#v", env.Context)
	}
	if !reflect.DeepEqual(env.Zones[0].InitialStates, map[string]interface{}{"humidity": 40.0}) {
		t.Errorf("room states did not become the initial states: %#v", env.Zones[0].InitialStates)
	}
	if !reflect.DeepEqual(env.Zones[0].Assets[0].InitialStates, map[string]interface{}{"kwh": 290508.57080252626}) {
		t.Errorf("device states did not become the initial states: %#v", env.Zones[0].Assets[0].InitialStates)
	}
}

func TestFromLegacyWorldDefaultsTypeZoneTypeAndAssetKind(t *testing.T) {
	env, _ := convert(t, legacyWorld())
	if env.Type != IndustrialSite {
		t.Errorf("the caller supplied type was not used: %q", env.Type)
	}
	if env.Zones[0].Type != ZoneHall {
		t.Errorf("expected zone type %q, got %q", ZoneHall, env.Zones[0].Type)
	}
	if env.Zones[0].Assets[0].Kind != AssetMachine {
		t.Errorf("expected asset kind %q, got %q", AssetMachine, env.Zones[0].Assets[0].Kind)
	}
}

func TestFromLegacyWorldRejectsAnUnknownEnvironmentType(t *testing.T) {
	_, _, err := FromLegacyWorld(legacyWorld(), EnvironmentType("factory"))
	if err == nil {
		t.Fatal("expected an error for an unknown environment type")
	}
}

func TestFromLegacyWorldSubstitutesEmptyNames(t *testing.T) {
	world := legacyWorld()
	world.Name = ""
	world.Rooms["room-key"].Name = "   "
	world.Rooms["room-key"].Devices["device-key"].Name = ""
	service := world.Rooms["room-key"].Devices["device-key"].Services["service-key"]
	service.Name = "\t\n"
	world.Rooms["room-key"].Devices["device-key"].Services["service-key"] = service

	env, problems := convert(t, world)
	got := []string{env.Name, env.Zones[0].Name, env.Zones[0].Assets[0].Name, firstChannel(t, env).Name}
	want := []string{"unnamed environment", "unnamed zone", "unnamed asset", "unnamed channel"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("empty names were not substituted: got %v, want %v", got, want)
	}
	if len(problems) != 4 {
		t.Errorf("expected one problem per substituted name, got:\n%v", problemMessages(problems))
	}
	if err := Validate(env); err != nil {
		t.Errorf("substituted names must leave a valid environment: %v", err)
	}
}

func TestFromLegacyWorldReportsAWorldWithoutRooms(t *testing.T) {
	world := legacyWorld()
	world.Rooms = nil
	env, problems := convert(t, world)
	if len(env.Zones) != 0 {
		t.Errorf("expected no zones, got %d", len(env.Zones))
	}
	assertProblemMentions(t, problems, "no rooms")
	//an environment without a zone stays invalid on purpose: inventing a zone
	//would invent structure the legacy document does not have
	if err := Validate(env); err == nil {
		t.Error("expected an environment without zones to be invalid")
	}
}

func TestFromLegacyWorldAcceptsADeviceWithoutServices(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices["device-key"].Services = nil
	env, problems := convert(t, world)
	assertNoProblems(t, problems)
	if got := env.Zones[0].Assets[0].Channels; len(got) != 0 {
		t.Errorf("expected no channels, got %#v", got)
	}
	if err := Validate(env); err != nil {
		t.Errorf("an asset without channels must be valid: %v", err)
	}
}

func TestFromLegacyWorldAcceptsARoomWithoutDevices(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices = nil
	env, problems := convert(t, world)
	assertNoProblems(t, problems)
	if got := env.Zones[0].Assets; len(got) != 0 {
		t.Errorf("expected no assets, got %#v", got)
	}
	if err := Validate(env); err != nil {
		t.Errorf("a zone without assets must be valid: %v", err)
	}
}

func TestFromLegacyWorldSkipsANullRoomAndDevice(t *testing.T) {
	world := legacyWorld()
	world.Rooms["null-room"] = nil
	world.Rooms["room-key"].Devices["null-device"] = nil
	env, problems := convert(t, world)
	if len(env.Zones) != 1 || len(env.Zones[0].Assets) != 1 {
		t.Errorf("expected the null entries to be skipped and the rest kept, got %#v", env.Zones)
	}
	assertProblemMentions(t, problems, "legacy:rooms[null-room]")
	assertProblemMentions(t, problems, "legacy:rooms[room-key].devices[null-device]")
}

func TestFromLegacyWorldReportsAMissingDeviceType(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices["device-key"].ExternalTypeId = ""
	env, problems := convert(t, world)
	assertProblemMentions(t, problems, "device type")
	if env.Zones[0].Assets[0].ExternalTypeId != "" {
		t.Errorf("a device type must never be invented, got %q", env.Zones[0].Assets[0].ExternalTypeId)
	}
}

func TestFromLegacyWorldSubstitutesAPlaceholderForAServiceWithoutCode(t *testing.T) {
	world := legacyWorld()
	service := world.Rooms["room-key"].Devices["device-key"].Services["service-key"]
	service.Code = "  "
	world.Rooms["room-key"].Devices["device-key"].Services["service-key"] = service

	env, problems := convert(t, world)
	assertProblemMentions(t, problems, "no code")
	//an empty script is rejected by Validate, so the document would not be
	//importable at all without the placeholder
	if err := Validate(env); err != nil {
		t.Errorf("a service without code must still leave a valid environment: %v", err)
	}
}

func TestFromLegacyWorldReportsTheDroppedDeviceImage(t *testing.T) {
	world := legacyWorld()
	world.Rooms["room-key"].Devices["device-key"].ImageUrl = "https://example.com/compressor.png"
	_, problems := convert(t, world)
	assertProblemMentions(t, problems, "https://example.com/compressor.png")
}

func TestFromLegacyWorldOrdersZonesAssetsAndChannelsDeterministically(t *testing.T) {
	world := legacyWorld()
	room := world.Rooms["room-key"]
	room.Devices["b-key"] = &state.Device{Id: "b", Name: "Bravo", ExternalTypeId: "urn:dt:1"}
	room.Devices["a-key"] = &state.Device{Id: "a", Name: "Alpha", ExternalTypeId: "urn:dt:1"}
	world.Rooms["second-room-key"] = &state.Room{Id: "room-2", Name: "Aaa Hall"}

	//map iteration is random, so the same input must be converted repeatedly to
	//show that the order of the result does not depend on it
	first, _ := convert(t, world)
	for run := 0; run < 20; run++ {
		again, _ := convert(t, world)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("conversion is not deterministic:\nfirst: %#v\nagain: %#v", first, again)
		}
	}
	names := []string{first.Zones[0].Name, first.Zones[1].Name}
	if !reflect.DeepEqual(names, []string{"Aaa Hall", "Fabric"}) {
		t.Errorf("zones are not ordered by name: %v", names)
	}
	assetNames := []string{}
	for _, asset := range first.Zones[1].Assets {
		assetNames = append(assetNames, asset.Name)
	}
	if !reflect.DeepEqual(assetNames, []string{"Alpha", "Bravo", "Compressor"}) {
		t.Errorf("assets are not ordered by name: %v", assetNames)
	}
}

func TestFromLegacyWorldDoesNotShareStateMapsWithTheLegacyWorld(t *testing.T) {
	world := legacyWorld()
	world.States["nested"] = map[string]interface{}{"inner": 1.0, "list": []interface{}{1.0}}
	env, _ := convert(t, world)

	//the legacy runtime keeps mutating its maps while it runs
	world.States["temperature"] = 99.0
	world.States["nested"].(map[string]interface{})["inner"] = 99.0
	world.States["nested"].(map[string]interface{})["list"].([]interface{})[0] = 99.0

	want := map[string]interface{}{
		"temperature": 21.5,
		"nested":      map[string]interface{}{"inner": 1.0, "list": []interface{}{1.0}},
	}
	if !reflect.DeepEqual(env.Context, want) {
		t.Errorf("the converted context changed with the legacy world: %#v", env.Context)
	}
}

func TestFromLegacyWorldDerivesTheSeedFromTheWorldId(t *testing.T) {
	first, _ := convert(t, legacyWorld())
	again, _ := convert(t, legacyWorld())
	if first.Seed != again.Seed {
		t.Errorf("the same world produced the seeds %d and %d, so a re-run of the migration would look like a change", first.Seed, again.Seed)
	}
	if first.Seed < 0 {
		t.Errorf("expected a non negative seed, got %d", first.Seed)
	}
	other := legacyWorld()
	other.Id = "world-2"
	otherEnv, _ := convert(t, other)
	if otherEnv.Seed == first.Seed {
		t.Errorf("two different worlds share the seed %d", first.Seed)
	}
}

// industryWorld is the real production export of the "Industry" world. The mongo
// shell wrappers of the export (ObjectId(...), NumberLong(...)) were stripped
// when the fixture was written, and the legacy _id was dropped: the rest is
// verbatim, including every platform reference.
func industryWorld(t *testing.T) state.World {
	t.Helper()
	content, err := os.ReadFile("testdata/industry-world.json")
	if err != nil {
		t.Fatal(err)
	}
	world := state.World{}
	err = json.Unmarshal(content, &world)
	if err != nil {
		t.Fatal(err)
	}
	//World.Owner is json:"-" (it comes from the token, not from the document),
	//so the exported owner has to be applied by hand
	world.Owner = "aae7e87b-63a2-477f-afb4-caa0db84e3fa"
	return world
}

func TestFromLegacyWorldConvertsTheIndustryExportIntoTheExpectedShape(t *testing.T) {
	env, _ := convert(t, industryWorld(t))
	zones, assets, channels := len(env.Zones), 0, 0
	for _, zone := range env.Zones {
		assets += len(zone.Assets)
		for _, asset := range zone.Assets {
			channels += len(asset.Channels)
		}
	}
	if zones != 1 || assets != 3 || channels != 24 {
		t.Errorf("expected 1 zone, 3 assets and 24 channels, got %d/%d/%d", zones, assets, channels)
	}
}

// The whole point of the migration, measured against the real export: every one
// of the 18 device routines finds its channel, and only the 3 room routines are
// left, which are not migrated by decision.
func TestFromLegacyWorldAttachesEveryDeviceRoutineOfTheIndustryExport(t *testing.T) {
	env, problems := convert(t, industryWorld(t))

	unmatched, notMigrated := 0, 0
	for _, problem := range problems {
		if strings.Contains(problem.Path, "change_routines[") {
			unmatched++
		}
		if strings.Contains(problem.Path, "change_routines_not_migrated[") {
			notMigrated++
		}
	}
	if unmatched != 0 {
		t.Errorf("expected every device routine to be attached, %d were not:\n%v", unmatched, problemMessages(problems))
	}
	if notMigrated != 3 {
		t.Errorf("expected the 3 room routines to be reported as not migrated, got %d", notMigrated)
	}

	withSource := 0
	for _, zone := range env.Zones {
		for _, asset := range zone.Assets {
			for _, channel := range asset.Channels {
				if channel.Source.IntervalSeconds > 0 {
					withSource++
				}
			}
		}
	}
	if withSource != 18 {
		t.Errorf("expected 18 channels to carry a routine on its own interval, got %d", withSource)
	}
}

func TestFromLegacyWorldReportsNothingButRoutinesForTheIndustryExport(t *testing.T) {
	_, problems := convert(t, industryWorld(t))
	for _, problem := range problems {
		if !strings.Contains(problem.Path, "change_routines") {
			t.Errorf("unexpected problem: %v", problem)
		}
	}
}

func TestFromLegacyWorldPreservesEveryPlatformReferenceOfTheIndustryExport(t *testing.T) {
	world := industryWorld(t)
	wantDevices, wantTypes, wantServices := []string{}, []string{}, []string{}
	for _, room := range world.Rooms {
		for _, device := range room.Devices {
			wantDevices = append(wantDevices, device.ExternalRef)
			wantTypes = append(wantTypes, device.ExternalTypeId)
			for _, service := range device.Services {
				wantServices = append(wantServices, service.ExternalRef)
			}
		}
	}

	env, _ := convert(t, world)
	gotDevices, gotTypes, gotServices := []string{}, []string{}, []string{}
	for _, zone := range env.Zones {
		for _, asset := range zone.Assets {
			gotDevices = append(gotDevices, asset.ExternalRef)
			gotTypes = append(gotTypes, asset.ExternalTypeId)
			for _, channel := range asset.Channels {
				gotServices = append(gotServices, channel.ExternalRef)
			}
		}
	}

	for _, pair := range []struct {
		what string
		want []string
		got  []string
	}{
		{what: "device", want: wantDevices, got: gotDevices},
		{what: "device type", want: wantTypes, got: gotTypes},
		{what: "service", want: wantServices, got: gotServices},
	} {
		sort.Strings(pair.want)
		sort.Strings(pair.got)
		if !reflect.DeepEqual(pair.want, pair.got) {
			t.Errorf("%v references were not preserved:\nwant %v\ngot  %v", pair.what, pair.want, pair.got)
		}
	}
}

func TestFromLegacyWorldConvertsTheIndustryExportIntoAValidEnvironment(t *testing.T) {
	env, _ := convert(t, industryWorld(t))
	if err := Validate(env); err != nil {
		t.Errorf("the converted production world is not valid: %v", err)
	}
}

func TestFromLegacyWorldMakesEveryIndustryChannelASensor(t *testing.T) {
	env, _ := convert(t, industryWorld(t))
	for _, zone := range env.Zones {
		for _, asset := range zone.Assets {
			for _, channel := range asset.Channels {
				if channel.Direction != Sensor || channel.IntervalSeconds <= 0 {
					t.Errorf("channel %v (%v) became %q with interval %d", channel.Name, channel.Id, channel.Direction, channel.IntervalSeconds)
				}
			}
		}
	}
}

// The migration is only faithful if both intervals survive it: every device
// routine's interval reappears as a source interval, and every legacy
// sensor_interval as a publish interval. Both sides are computed from the real
// export rather than written down, so the test cannot drift from the fixture.
func TestFromLegacyWorldPreservesBothIntervalsOfTheIndustryExport(t *testing.T) {
	world := industryWorld(t)

	wantSource, wantPublish := []int64{}, []int64{}
	for _, room := range world.Rooms {
		for _, device := range room.Devices {
			for _, routine := range device.ChangeRoutines {
				wantSource = append(wantSource, routine.Interval)
			}
			for _, service := range device.Services {
				wantPublish = append(wantPublish, service.SensorInterval)
			}
		}
	}

	env, _ := convert(t, world)
	gotSource, gotPublish := []int64{}, []int64{}
	for _, zone := range env.Zones {
		for _, asset := range zone.Assets {
			for _, channel := range asset.Channels {
				if channel.Source.IntervalSeconds > 0 {
					gotSource = append(gotSource, channel.Source.IntervalSeconds)
				}
				gotPublish = append(gotPublish, channel.IntervalSeconds)
			}
		}
	}

	//a comparison of two empty lists would pass and prove nothing
	if len(wantSource) != 18 || len(wantPublish) != 24 {
		t.Fatalf("the fixture changed: expected 18 routines and 24 services, got %d and %d", len(wantSource), len(wantPublish))
	}
	if !sameIntervals(wantSource, gotSource) {
		t.Errorf("the routine intervals were not preserved as source intervals:\nlegacy %v\nsource %v",
			sortedIntervals(wantSource), sortedIntervals(gotSource))
	}
	if !sameIntervals(wantPublish, gotPublish) {
		t.Errorf("the sensor intervals were not preserved as publish intervals:\nlegacy  %v\npublish %v",
			sortedIntervals(wantPublish), sortedIntervals(gotPublish))
	}
}

func sortedIntervals(in []int64) []int64 {
	out := append([]int64{}, in...)
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

func sameIntervals(a []int64, b []int64) bool {
	return reflect.DeepEqual(sortedIntervals(a), sortedIntervals(b))
}

func TestParseWindow(t *testing.T) {
	for _, tc := range []struct {
		window string
		hours  float64
	}{
		{"36h", 36}, {"7d", 168}, {"4w", 672}, {"90m", 1.5}, {"1.5d", 36},
		{"1y", 8760}, {"0.5y", 4380},
		//the longest window a time.Duration can carry; it has to keep parsing,
		//because the overflow guard closes an overflow and does not introduce a
		//shorter limit of its own
		{"292y", 292 * 365 * 24},
	} {
		got, err := ParseWindow(tc.window)
		if err != nil || got != time.Duration(tc.hours*float64(time.Hour)) {
			t.Errorf("%q: expected %vh, got %v (%v)", tc.window, tc.hours, got, err)
		}
	}
	for _, bad := range []string{"", "d", "-3h", "0h", "3x", "sieben"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q has to be refused", bad)
		}
	}
}

// A count is read as a float64, which carries far more range than the int64
// behind a time.Duration. Converting an out of range float is undefined and on
// the usual machines lands on the most negative duration there is - so "293y"
// used to parse without an error and hand the platform origin a negative
// window, whose end.Add(-window) queries from three centuries in the future.
//
// The same conversion is undefined for a NaN, which "nan" is a spelling of and
// which compares false against every bound, so it needs the negated form of
// the positivity check rather than a second comparison.
func TestParseWindowRefusesWhatATimeDurationCannotHold(t *testing.T) {
	//refused as too long, and asserted on the reason rather than only on the
	//presence of an error: what an out of range conversion produces differs by
	//architecture - amd64 lands on the most negative duration, others saturate
	//at the largest positive one - so a test that only demands "some error"
	//passes on this machine without the range check and fails on another.
	for _, tooLong := range []string{
		"293y", "300y", "1e15d", "1e300w", //beyond int64
		"infd", "Infinityy", //parse as +Inf
	} {
		got, err := ParseWindow(tooLong)
		if err == nil {
			t.Errorf("%q has to be refused, got %v", tooLong, got)
			continue
		}
		if !strings.Contains(err.Error(), "longer than the clock can hold") {
			t.Errorf("%q has to be refused for its length, got %v", tooLong, err)
		}
		if got != 0 {
			t.Errorf("%q: a refused window has to come back as 0, got %v", tooLong, got)
		}
	}
	//refused as no usable count at all
	for _, bad := range []string{
		"nand", "NaNy", //parse as NaN and compare false against every bound
		"1e-30d", //rounds away to no window at all
	} {
		got, err := ParseWindow(bad)
		if err == nil {
			t.Errorf("%q has to be refused, got %v", bad, got)
			continue
		}
		if got != 0 {
			t.Errorf("%q: a refused window has to come back as 0, got %v", bad, got)
		}
	}
	//the failure this guards against is a window that is not positive; assert
	//it on the largest value that still passes rather than only on the message
	if got, err := ParseWindow("292y"); err != nil || got <= 0 {
		t.Errorf("292y has to parse into a positive window, got %v (%v)", got, err)
	}
}
