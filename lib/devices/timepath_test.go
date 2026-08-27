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

package devices

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/converter/lib/converter/characteristics"
	"github.com/SENERGY-Platform/models/go/models"
)

const energyCharacteristic = "urn:infai:ses:characteristic:energy"

// timedService is a device type service shaped the way a backfillable one has to
// be: one json output whose root is a record with the measured value and the
// event time beside each other, and the attribute that tells the platform to
// read the time out of the payload.
func timedService(timePath string, members ...models.ContentVariable) models.Service {
	return models.Service{
		Id:         "urn:infai:ses:service:1",
		Name:       "reading",
		Attributes: []models.Attribute{{Key: TimePathAttribute, Value: timePath}},
		Outputs: []models.Content{{
			Serialization: models.JSON,
			ContentVariable: models.ContentVariable{
				Name:                "root",
				Type:                models.Structure,
				SubContentVariables: members,
			},
		}},
	}
}

func valueMember(name string) models.ContentVariable {
	return models.ContentVariable{Name: name, Type: models.Float, CharacteristicId: energyCharacteristic}
}

func timeMember(name string, characteristic string) models.ContentVariable {
	return models.ContentVariable{Name: name, Type: models.Integer, CharacteristicId: characteristic}
}

func TestResolveTimeShapeAcceptsTheUnitsThePlatformCanRead(t *testing.T) {
	for name, testCase := range map[string]struct {
		characteristic string
		unit           TimeUnit
	}{
		"seconds":      {characteristics.UnixSeconds, TimeUnitSeconds},
		"milliseconds": {characteristics.UnixMilliSeconds, TimeUnitMilliseconds},
	} {
		t.Run(name, func(t *testing.T) {
			service := timedService("root.time", valueMember("value"), timeMember("time", testCase.characteristic))
			shape, err := ResolveTimeShape(service)
			if err != nil {
				t.Fatalf("expected the service to be usable, got %v", err)
			}
			if shape.RootName != "root" {
				t.Errorf("expected the root name root, got %v", shape.RootName)
			}
			if !reflect.DeepEqual(shape.ValuePath, []string{"value"}) {
				t.Errorf("expected the value at [value], got %v", shape.ValuePath)
			}
			if !reflect.DeepEqual(shape.TimePath, []string{"time"}) {
				t.Errorf("expected the time at [time], got %v", shape.TimePath)
			}
			if shape.TimeUnit != testCase.unit {
				t.Errorf("expected the unit %v, got %v", testCase.unit, shape.TimeUnit)
			}
		})
	}
}

// TestResolveTimeShapeSkipsTheTimeWhenLookingForTheValue: the value is found as
// the first leaf carrying a characteristic, and the time carries one too. A
// service that declares the time first would otherwise have its reading
// published into the time column and its time left empty.
func TestResolveTimeShapeSkipsTheTimeWhenLookingForTheValue(t *testing.T) {
	service := timedService("root.time",
		timeMember("time", characteristics.UnixSeconds),
		valueMember("value"))
	shape, err := ResolveTimeShape(service)
	if err != nil {
		t.Fatalf("expected the service to be usable, got %v", err)
	}
	if !reflect.DeepEqual(shape.ValuePath, []string{"value"}) {
		t.Errorf("expected the value at [value] even though the time comes first, got %v", shape.ValuePath)
	}
}

func TestResolveTimeShapeWalksNestedPaths(t *testing.T) {
	service := models.Service{
		Attributes: []models.Attribute{{Key: TimePathAttribute, Value: "root.meta.taken_at"}},
		Outputs: []models.Content{{
			Serialization: models.JSON,
			ContentVariable: models.ContentVariable{
				Name: "root", Type: models.Structure,
				SubContentVariables: []models.ContentVariable{
					{Name: "meta", Type: models.Structure, SubContentVariables: []models.ContentVariable{
						timeMember("taken_at", characteristics.UnixMilliSeconds),
					}},
					{Name: "reading", Type: models.Structure, SubContentVariables: []models.ContentVariable{
						valueMember("kwh"),
					}},
				},
			},
		}},
	}
	shape, err := ResolveTimeShape(service)
	if err != nil {
		t.Fatalf("expected the service to be usable, got %v", err)
	}
	if !reflect.DeepEqual(shape.TimePath, []string{"meta", "taken_at"}) {
		t.Errorf("expected the time at [meta taken_at], got %v", shape.TimePath)
	}
	if !reflect.DeepEqual(shape.ValuePath, []string{"reading", "kwh"}) {
		t.Errorf("expected the value at [reading kwh], got %v", shape.ValuePath)
	}
}

func TestResolveTimeShapeReportsTheOrdinaryCase(t *testing.T) {
	service := models.Service{Outputs: []models.Content{{Serialization: models.JSON}}}
	if _, err := ResolveTimeShape(service); !errors.Is(err, ErrNoTimePath) {
		t.Fatalf("expected ErrNoTimePath for a service without the attribute, got %v", err)
	}
	//an attribute that is present but empty is the same thing: the ingestion
	//requires len(attr.Value) > 0 before it looks at it
	service.Attributes = []models.Attribute{{Key: TimePathAttribute, Value: ""}}
	if _, err := ResolveTimeShape(service); !errors.Is(err, ErrNoTimePath) {
		t.Fatalf("expected ErrNoTimePath for an empty attribute, got %v", err)
	}
}

// TestResolveTimeShapeRefusesWhatThePlatformCannotIngest pins every rejection
// with the reason it exists for. Each of these would otherwise reach the
// platform and either lose the row or, for nanoseconds, take the connector
// process down; TestThePlatformReallyCannotIngestTheRefusedUnits proves the two
// unit cases against the dependency itself.
func TestResolveTimeShapeRefusesWhatThePlatformCannotIngest(t *testing.T) {
	for name, testCase := range map[string]struct {
		service  models.Service
		contains string
	}{
		"nanoseconds": {
			timedService("root.time", valueMember("value"), timeMember("time", characteristics.UnixNanoSeconds)),
			"nanoseconds",
		},
		"iso timestamp": {
			timedService("root.time", valueMember("value"),
				models.ContentVariable{Name: "time", Type: models.String, CharacteristicId: characteristics.IsoTimestamp}),
			"iso timestamp",
		},
		"time without a characteristic": {
			timedService("root.time", valueMember("value"),
				models.ContentVariable{Name: "time", Type: models.Integer}),
			"no characteristic",
		},
		"time is not a number": {
			timedService("root.time", valueMember("value"),
				models.ContentVariable{Name: "time", Type: models.String, CharacteristicId: characteristics.UnixSeconds}),
			"has to be a number",
		},
		"value is not a number": {
			timedService("root.time",
				models.ContentVariable{Name: "value", Type: models.String, CharacteristicId: energyCharacteristic},
				timeMember("time", characteristics.UnixSeconds)),
			"publishes a number",
		},
		"no value beside the time": {
			timedService("root.time", timeMember("time", characteristics.UnixSeconds)),
			"no content variable beside the time",
		},
		"path does not resolve": {
			timedService("root.taken_at", valueMember("value"), timeMember("time", characteristics.UnixSeconds)),
			"does not resolve",
		},
		"path names a whole output": {
			timedService("root", valueMember("value"), timeMember("time", characteristics.UnixSeconds)),
			"names a whole output",
		},
		"root name does not match": {
			timedService("other.time", valueMember("value"), timeMember("time", characteristics.UnixSeconds)),
			"the output's root variable is",
		},
		"structure without members": {
			models.Service{
				Attributes: []models.Attribute{{Key: TimePathAttribute, Value: "root.a.time"}},
				Outputs: []models.Content{{Serialization: models.JSON, ContentVariable: models.ContentVariable{
					Name: "root", Type: models.Structure,
					SubContentVariables: []models.ContentVariable{{Name: "a", Type: models.Structure}},
				}}},
			},
			"structure without members",
		},
		"map instead of a record": {
			models.Service{
				Attributes: []models.Attribute{{Key: TimePathAttribute, Value: "root.a.time"}},
				Outputs: []models.Content{{Serialization: models.JSON, ContentVariable: models.ContentVariable{
					Name: "root", Type: models.Structure,
					SubContentVariables: []models.ContentVariable{{Name: "a", Type: models.Structure,
						SubContentVariables: []models.ContentVariable{{Name: "*", Type: models.Float}}}},
				}}},
			},
			"is a map",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveTimeShape(testCase.service)
			if err == nil {
				t.Fatalf("expected a rejection, got a usable shape")
			}
			if errors.Is(err, ErrNoTimePath) {
				t.Fatalf("expected a rejection naming the problem, got the no-time-path case")
			}
			if !strings.Contains(err.Error(), testCase.contains) {
				t.Errorf("expected the reason to mention %q, got %q", testCase.contains, err.Error())
			}
		})
	}
}

func TestResolveTimeShapeRefusesAnOutputMosesCannotFill(t *testing.T) {
	twoOutputs := timedService("root.time", valueMember("value"), timeMember("time", characteristics.UnixSeconds))
	twoOutputs.Outputs = append(twoOutputs.Outputs, models.Content{
		Serialization:   models.JSON,
		ContentVariable: models.ContentVariable{Name: "second", Type: models.Float},
	})
	if _, err := ResolveTimeShape(twoOutputs); err == nil || !strings.Contains(err.Error(), "outputs") {
		t.Errorf("expected a service with two outputs to be refused, got %v", err)
	}

	notJson := timedService("root.time", valueMember("value"), timeMember("time", characteristics.UnixSeconds))
	notJson.Outputs[0].Serialization = models.XML
	if _, err := ResolveTimeShape(notJson); err == nil || !strings.Contains(err.Error(), "json") {
		t.Errorf("expected an xml output to be refused, got %v", err)
	}
}

func TestPayloadPutsValueAndTimeAtTheirPaths(t *testing.T) {
	//not a round number of seconds, so a unit that truncates is visible
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_000_000, time.UTC)

	seconds := TimeShape{RootName: "root", ValuePath: []string{"value"}, TimePath: []string{"time"}, TimeUnit: TimeUnitSeconds}
	want := map[string]interface{}{"value": 42.5, "time": at.Unix()}
	if got := seconds.Payload(42.5, at); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}

	millis := TimeShape{RootName: "root", ValuePath: []string{"value"}, TimePath: []string{"time"}, TimeUnit: TimeUnitMilliseconds}
	want = map[string]interface{}{"value": 42.5, "time": at.UnixMilli()}
	if got := millis.Payload(42.5, at); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}
}

func TestPayloadBuildsTheContainersOfANestedPath(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	shape := TimeShape{
		RootName:  "root",
		ValuePath: []string{"reading", "kwh"},
		TimePath:  []string{"meta", "taken_at"},
		TimeUnit:  TimeUnitMilliseconds,
	}
	want := map[string]interface{}{
		"reading": map[string]interface{}{"kwh": 7.25},
		"meta":    map[string]interface{}{"taken_at": at.UnixMilli()},
	}
	if got := shape.Payload(7.25, at); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}
}

// TestPayloadSharesAContainerBetweenValueAndTime: value and time in the same
// nested record must not overwrite each other's container.
func TestPayloadSharesAContainerBetweenValueAndTime(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	shape := TimeShape{
		RootName:  "root",
		ValuePath: []string{"m", "kwh"},
		TimePath:  []string{"m", "t"},
		TimeUnit:  TimeUnitSeconds,
	}
	want := map[string]interface{}{"m": map[string]interface{}{"kwh": 1.5, "t": at.Unix()}}
	if got := shape.Payload(1.5, at); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}
}
