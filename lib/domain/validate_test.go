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
	"errors"
	"math"
	"strings"
	"testing"
)

func validEnvironment() Environment {
	return Environment{
		Id:      "env-1",
		Name:    "Metallbau Musterstadt",
		Type:    IndustrialSite,
		Seed:    42,
		Context: map[string]interface{}{"outdoor_temperature": 12.5},
		Zones: []Zone{{
			Id:   "zone-hall",
			Name: "Halle 1",
			Type: ZoneHall,
			Assets: []Asset{{
				Id:             "asset-meter",
				Name:           "Hauptzähler Strom",
				Kind:           AssetMeter,
				ExternalRef:    "urn:infai:ses:device:abc",
				ExternalTypeId: "urn:infai:ses:device-type:abc",
				Channels: []Channel{{
					Id:               "channel-energy",
					Name:             "Wirkenergie",
					Direction:        Sensor,
					ExternalRef:      "urn:infai:ses:service:abc",
					CharacteristicId: "urn:infai:ses:characteristic:kwh",
					Unit:             "kWh",
					IntervalSeconds:  30,
					Source:           Source{Kind: SourceScript, Script: &ScriptSource{Code: "moses.service.send(1);"}},
				}},
			}},
		}},
	}
}

func problemPaths(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
	paths := []string{}
	for _, p := range verr.Problems {
		paths = append(paths, p.Path)
	}
	return paths
}

func assertHasPath(t *testing.T, err error, want string) {
	t.Helper()
	paths := problemPaths(t, err)
	for _, p := range paths {
		if p == want {
			return
		}
	}
	t.Fatalf("expected a problem at %q, got %v", want, paths)
}

func TestValidateAcceptsACompleteEnvironment(t *testing.T) {
	if err := Validate(validEnvironment()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsAnUnknownEnvironmentType(t *testing.T) {
	env := validEnvironment()
	env.Type = "factory"
	assertHasPath(t, Validate(env), "type")
}

func TestValidateRejectsAnEnvironmentWithoutZones(t *testing.T) {
	env := validEnvironment()
	env.Zones = nil
	assertHasPath(t, Validate(env), "zones")
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	env := validEnvironment()
	env.Name = " "
	env.Type = "nope"
	env.Zones[0].Name = ""
	err := Validate(env)
	paths := problemPaths(t, err)
	if len(paths) < 3 {
		t.Fatalf("expected at least 3 problems so a document can be fixed in one pass, got %v", paths)
	}
}

func TestValidateRejectsADuplicateIdAcrossDifferentEntities(t *testing.T) {
	env := validEnvironment()
	// a zone and a channel sharing an id would make an update ambiguous
	env.Zones[0].Assets[0].Channels[0].Id = env.Zones[0].Id
	err := Validate(env)
	if err == nil {
		t.Fatal("expected a duplicate id to be rejected")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("expected the message to name the duplicate, got %v", err)
	}
}

func TestValidateAllowsEmptyIdsBecauseTheServerAssignsThem(t *testing.T) {
	env := validEnvironment()
	env.Id = ""
	env.Zones[0].Id = ""
	env.Zones[0].Assets[0].Id = ""
	env.Zones[0].Assets[0].Channels[0].Id = ""
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsZonesNestedTooDeep(t *testing.T) {
	env := validEnvironment()
	deepest := &env.Zones[0]
	for i := 0; i <= MaxZoneDepth; i++ {
		deepest.Zones = []Zone{{Id: "", Name: "tief", Type: ZoneRoom}}
		deepest = &deepest.Zones[0]
	}
	if err := Validate(env); err == nil {
		t.Fatal("expected excessive nesting to be rejected")
	}
}

func TestValidateAcceptsNestingUpToTheLimit(t *testing.T) {
	env := Environment{
		Id:    "env",
		Name:  "Mehrfamilienhaus",
		Type:  ApartmentBuilding,
		Zones: []Zone{{Name: "Gebäude", Type: ZoneBuilding}},
	}
	// building > floor > unit > room is the realistic case and must fit
	env.Zones[0].Zones = []Zone{{Name: "Etage 2", Type: ZoneFloor,
		Zones: []Zone{{Name: "Wohnung 2.1", Type: ZoneUnit,
			Zones: []Zone{{Name: "Bad", Type: ZoneRoom}}}}}}
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRequiresADeviceTypeOnAnAsset(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].ExternalTypeId = ""
	assertHasPath(t, Validate(env), "zones[0].assets[0].external_type_id")
}

func TestValidateAllowsAnAssetWithoutAPlatformDeviceYet(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].ExternalRef = ""
	if err := Validate(env); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsAnIntervalOnAnActuator(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].Direction = Actuator
	env.Zones[0].Assets[0].Channels[0].IntervalSeconds = 30
	assertHasPath(t, Validate(env), "zones[0].assets[0].channels[0].interval_seconds")
}

func TestValidateRejectsANegativeInterval(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].IntervalSeconds = -1
	assertHasPath(t, Validate(env), "zones[0].assets[0].channels[0].interval_seconds")
}

func TestValidateRejectsAnEmptyScript(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].Source.Script.Code = "  "
	assertHasPath(t, Validate(env), "zones[0].assets[0].channels[0].source.script.code")
}

// Every declared kind executes now; what stays refused is a kind whose variant
// is missing, and the two dataset origins nothing serves.
//
// SourceAggregate is deliberately not in the list: it is the one kind with no
// variant of its own, because its inputs are the meter tree rather than a
// configuration block. The opposite rule holds for it and is pinned in
// validate_aggregate_test.go - a variant next to kind aggregate is refused.
func TestValidateRejectsAKindWithoutItsVariant(t *testing.T) {
	for _, kind := range []SourceKind{SourceProfile, SourceDataset, SourceFormula} {
		env := validEnvironment()
		env.Zones[0].Assets[0].Channels[0].Source = Source{Kind: kind}
		err := Validate(env)
		if err == nil || !strings.Contains(err.Error(), "must be set when kind is") {
			t.Fatalf("expected kind %q to demand its variant, got %v", kind, err)
		}
	}
}

func TestValidateRejectsMoreThanOneSourceVariant(t *testing.T) {
	env := validEnvironment()
	env.Zones[0].Assets[0].Channels[0].Source.Dataset = &DatasetSource{Origin: OriginFile}
	assertHasPath(t, Validate(env), "zones[0].assets[0].channels[0].source")
}

func TestValidateRejectsNonFiniteStateValues(t *testing.T) {
	for name, value := range map[string]float64{"NaN": math.NaN(), "Inf": math.Inf(1)} {
		env := validEnvironment()
		env.Context = map[string]interface{}{"outdoor_temperature": value}
		if err := Validate(env); err == nil {
			t.Fatalf("expected %v to be rejected instead of silently becoming zero", name)
		}
	}
}

func TestValidateRejectsStateKeysMongoCannotStore(t *testing.T) {
	for _, key := range []string{"a.b", "$set", ""} {
		env := validEnvironment()
		env.Zones[0].InitialStates = map[string]interface{}{key: 1.0}
		if err := Validate(env); err == nil {
			t.Fatalf("expected the key %q to be rejected", key)
		}
	}
}
