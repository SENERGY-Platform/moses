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

func timeTextMember(name string, characteristic string) models.ContentVariable {
	return models.ContentVariable{Name: name, Type: models.String, CharacteristicId: characteristic}
}

// TestResolveTimeShapeAcceptsEveryTimeThePlatformCanRead covers all four time
// characteristics. Nanoseconds and the iso timestamp were refused here until
// platform-connector-lib c8133d0 repaired the ingestion; lib/devices/ingestion_test.go
// holds them to that against the dependency itself.
func TestResolveTimeShapeAcceptsEveryTimeThePlatformCanRead(t *testing.T) {
	for name, testCase := range map[string]struct {
		timeVariable models.ContentVariable
		encoding     TimeEncoding
	}{
		"seconds":      {timeMember("time", characteristics.UnixSeconds), TimeAsUnixSeconds},
		"milliseconds": {timeMember("time", characteristics.UnixMilliSeconds), TimeAsUnixMilliseconds},
		"nanoseconds":  {timeMember("time", characteristics.UnixNanoSeconds), TimeAsUnixNanoseconds},
		"nanoseconds as a string": {
			timeTextMember("time", characteristics.UnixNanoSeconds), TimeAsUnixNanosecondText},
		"iso timestamp": {timeTextMember("time", characteristics.IsoTimestamp), TimeAsIsoTimestamp},
		//a float declaration is a number too, which is what the ingestion reads
		"seconds declared as a float": {
			models.ContentVariable{Name: "time", Type: models.Float, CharacteristicId: characteristics.UnixSeconds},
			TimeAsUnixSeconds},
	} {
		t.Run(name, func(t *testing.T) {
			service := timedService("root.time", valueMember("value"), testCase.timeVariable)
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
			if shape.TimeEncoding != testCase.encoding {
				t.Errorf("expected the encoding %v, got %v", testCase.encoding, shape.TimeEncoding)
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
// platform and lose the row or the message.
//
// What is left of the two characteristics that used to be refused outright is a
// declaration check: the ingestion reads a unix time out of a number and an iso
// timestamp out of a string, so a variable typed as the other one cannot carry
// the time no matter which characteristic it names.
func TestResolveTimeShapeRefusesWhatThePlatformCannotIngest(t *testing.T) {
	for name, testCase := range map[string]struct {
		service  models.Service
		contains string
	}{
		"time without a characteristic": {
			timedService("root.time", valueMember("value"),
				models.ContentVariable{Name: "time", Type: models.Integer}),
			"no characteristic",
		},
		"seconds declared as a string": {
			timedService("root.time", valueMember("value"),
				timeTextMember("time", characteristics.UnixSeconds)),
			"in seconds has to be a number",
		},
		"milliseconds declared as a string": {
			timedService("root.time", valueMember("value"),
				timeTextMember("time", characteristics.UnixMilliSeconds)),
			"in milliseconds has to be a number",
		},
		"iso timestamp declared as a number": {
			timedService("root.time", valueMember("value"),
				timeMember("time", characteristics.IsoTimestamp)),
			"iso timestamp has to be a string",
		},
		"nanoseconds declared as a boolean": {
			timedService("root.time", valueMember("value"),
				models.ContentVariable{Name: "time", Type: models.Boolean, CharacteristicId: characteristics.UnixNanoSeconds}),
			"in nanoseconds has to be a number or its digits as a string",
		},
		"a characteristic that is not a time": {
			timedService("root.time", valueMember("value"),
				timeMember("time", energyCharacteristic)),
			"is not a time the platform can read",
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

// TestPayloadPutsValueAndTimeAtTheirPaths pins the wire shape of every encoding,
// down to the go type: a number that arrived as a string, or the other way
// round, is a row the ingestion cannot read.
func TestPayloadPutsValueAndTimeAtTheirPaths(t *testing.T) {
	//sub-second and sub-millisecond digits, so an encoding that truncates is
	//visible rather than accidentally right
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC)

	for name, testCase := range map[string]struct {
		encoding TimeEncoding
		want     interface{}
	}{
		"seconds":                 {TimeAsUnixSeconds, at.Unix()},
		"milliseconds":            {TimeAsUnixMilliseconds, at.UnixMilli()},
		"nanoseconds":             {TimeAsUnixNanoseconds, at.UnixNano()},
		"nanoseconds as a string": {TimeAsUnixNanosecondText, "1773500966535897123"},
		"iso timestamp":           {TimeAsIsoTimestamp, "2026-03-14T15:09:26.535897123Z"},
	} {
		t.Run(name, func(t *testing.T) {
			shape := TimeShape{RootName: "root", ValuePath: []string{"value"},
				TimePath: []string{"time"}, TimeEncoding: testCase.encoding}
			want := map[string]interface{}{"value": 42.5, "time": testCase.want}
			if got := shape.Payload(42.5, at); !reflect.DeepEqual(got, want) {
				t.Errorf("expected %#v, got %#v", want, got)
			}
		})
	}
}

// TestPayloadWritesAnIsoTimestampInUtc: the instant is what matters, but the
// text must not depend on where moses runs - two servers in different zones have
// to put the same bytes on the wire for the same reading.
func TestPayloadWritesAnIsoTimestampInUtc(t *testing.T) {
	shape := TimeShape{RootName: "root", ValuePath: []string{"value"},
		TimePath: []string{"time"}, TimeEncoding: TimeAsIsoTimestamp}
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)

	east := shape.Payload(1, at.In(time.FixedZone("east", 9*60*60)))["time"]
	west := shape.Payload(1, at.In(time.FixedZone("west", -5*60*60)))["time"]
	if east != west {
		t.Errorf("expected the same text from either zone, got %v and %v", east, west)
	}
	if east != "2026-03-14T15:09:26Z" {
		t.Errorf("expected the utc form, got %v", east)
	}
}

// TestPayloadDropsNoDigitsOfAnIsoTimestamp: RFC3339Nano omits trailing zeros of
// the fractional second, which is fine, but it must not omit leading ones - a
// timestamp 4 ns past the second is .000000004 and not .4.
func TestPayloadDropsNoDigitsOfAnIsoTimestamp(t *testing.T) {
	shape := TimeShape{RootName: "root", ValuePath: []string{"value"},
		TimePath: []string{"time"}, TimeEncoding: TimeAsIsoTimestamp}

	for _, testCase := range []struct {
		nanosecond int
		want       string
	}{
		{0, "2026-03-14T15:09:26Z"},
		{4, "2026-03-14T15:09:26.000000004Z"},
		{500_000_000, "2026-03-14T15:09:26.5Z"},
		{999_999_999, "2026-03-14T15:09:26.999999999Z"},
	} {
		at := time.Date(2026, 3, 14, 15, 9, 26, testCase.nanosecond, time.UTC)
		if got := shape.Payload(1, at)["time"]; got != testCase.want {
			t.Errorf("expected %v for %d ns, got %v", testCase.want, testCase.nanosecond, got)
		}
		//and whatever it wrote, the ingestion's own layout has to read it back
		parsed, err := time.Parse(time.RFC3339, shape.Payload(1, at)["time"].(string))
		if err != nil {
			t.Fatalf("the ingestion could not parse %v: %v", shape.Payload(1, at)["time"], err)
		}
		if parsed.UnixNano() != at.UnixNano() {
			t.Errorf("expected %v to read back as %v, got %v", testCase.want, at.UnixNano(), parsed.UnixNano())
		}
	}
}

func TestPayloadBuildsTheContainersOfANestedPath(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	shape := TimeShape{
		RootName:     "root",
		ValuePath:    []string{"reading", "kwh"},
		TimePath:     []string{"meta", "taken_at"},
		TimeEncoding: TimeAsUnixMilliseconds,
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
		RootName:     "root",
		ValuePath:    []string{"m", "kwh"},
		TimePath:     []string{"m", "t"},
		TimeEncoding: TimeAsUnixSeconds,
	}
	want := map[string]interface{}{"m": map[string]interface{}{"kwh": 1.5, "t": at.Unix()}}
	if got := shape.Payload(1.5, at); !reflect.DeepEqual(got, want) {
		t.Errorf("expected %#v, got %#v", want, got)
	}
}
