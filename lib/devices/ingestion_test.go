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
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/SENERGY-Platform/converter/lib/converter"
	"github.com/SENERGY-Platform/converter/lib/converter/characteristics"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/platform-connector-lib/marshalling"
	"github.com/SENERGY-Platform/platform-connector-lib/msgvalidation"
)

// The tests below stand in for an integration test against timescale. The test
// stack of this repository (lib/test/server) brings up kafka, mongodb,
// memcached, the device-repository and permissions-v2, but no postgres, so
// nothing here could observe a written row. What can be observed without one is
// the part that decides the row's timestamp, and every piece of it except a ten
// line flatten is exported: the connector's marshaller, its message cleaning and
// the converter's cast are all called here as the platform calls them.
//
// That is the whole point of these tests. They pin why ResolveTimeShape refuses
// two of the four time characteristics, against the dependency rather than
// against a comment - so a version bump that repairs either case fails here and
// prompts the refusal to be lifted, instead of leaving it in place forever.

// flattenLikeThePlatform is a verbatim copy of `flatten` in
// platform-connector-lib psql/publisher.go at
// v0.0.0-20260826082643-802ca9df203c. It is copied because it is unexported and
// because its string handling - the single quotes it adds for the sql literal -
// is exactly what the iso timestamp case founders on.
func flattenLikeThePlatform(m map[string]interface{}) (values map[string]interface{}) {
	values = make(map[string]interface{})
	for k, v := range m {
		switch child := v.(type) {
		case map[string]interface{}:
			nm := flattenLikeThePlatform(child)
			for nk, nv := range nm {
				values[k+"."+nk] = nv
			}
		case string:
			values[k] = "'" + v.(string) + "'"
		default:
			values[k] = v
		}
	}
	return values
}

// ingest runs one reading through the platform's own code, from the bytes moses
// puts on the wire to the nanosecond timestamp the ingestion stamps the row
// with. It mirrors handleDeviceEvent -> unmarshalMsg -> CleanMsg ->
// psql.Publish.
func ingest(t *testing.T, service models.Service, shape TimeShape, value float64, at time.Time) (interface{}, error) {
	t.Helper()

	//what connectorPublisher.PublishEventAt puts into the protocol segment
	segment, err := json.Marshal(shape.Payload(value, at))
	if err != nil {
		t.Fatal(err)
	}

	//unmarshalMsg: the segment is parsed against the output's root variable and
	//filed under that variable's name
	output := service.Outputs[0]
	marshaller, known := marshalling.Get(string(output.Serialization))
	if !known {
		t.Fatalf("no marshaller for %v", output.Serialization)
	}
	parsed, err := marshaller.Unmarshal(string(segment), output.ContentVariable)
	if err != nil {
		t.Fatalf("the platform could not parse what moses sends: %v", err)
	}
	message := map[string]interface{}{output.ContentVariable.Name: parsed}

	//CleanMsg: unknown fields out, missing ones defaulted
	message, err = msgvalidation.Clean(message, service)
	if err != nil {
		t.Fatalf("the platform rejected the message: %v", err)
	}

	//psql.Publish: flatten, look the time up by its full dotted path, cast it
	flat := flattenLikeThePlatform(message)
	timePath := output.ContentVariable.Name + "." + strings.Join(shape.TimePath, ".")
	timeValue, found := flat[timePath]
	if !found {
		t.Fatalf("the ingestion would not find a value at %v; it has %#v", timePath, flat)
	}
	timeVariable, err := resolveVariable(output.ContentVariable, shape.TimePath)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := converter.New()
	if err != nil {
		t.Fatal(err)
	}
	return conv.Cast(timeValue, timeVariable.CharacteristicId, characteristics.UnixNanoSeconds)
}

// TestThePlatformStampsTheRowWithTheInstantMosesSends is the contract the whole
// backfill rests on: what comes out at the far end is the instant that went in,
// down to the resolution of the declared unit.
func TestThePlatformStampsTheRowWithTheInstantMosesSends(t *testing.T) {
	//deliberately not a round second: a unit that silently truncated would
	//otherwise be indistinguishable from one that did not
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_897_000, time.UTC)

	for name, testCase := range map[string]struct {
		characteristic string
		want           time.Time
	}{
		"seconds":      {characteristics.UnixSeconds, at.Truncate(time.Second)},
		"milliseconds": {characteristics.UnixMilliSeconds, at.Truncate(time.Millisecond)},
	} {
		t.Run(name, func(t *testing.T) {
			service := timedService("root.time", valueMember("value"), timeMember("time", testCase.characteristic))
			shape, err := ResolveTimeShape(service)
			if err != nil {
				t.Fatalf("expected the service to be usable, got %v", err)
			}
			out, err := ingest(t, service, shape, 42.5, at)
			if err != nil {
				t.Fatalf("the ingestion could not read the time moses sent: %v", err)
			}
			//the ingestion asserts exactly this type, without checking
			nanos, isInt64 := out.(int64)
			if !isInt64 {
				t.Fatalf("the ingestion asserts int64 and would panic on %T", out)
			}
			if got := time.Unix(0, nanos).UTC(); !got.Equal(testCase.want) {
				t.Errorf("expected the row to be stamped %v, got %v", testCase.want, got)
			}
		})
	}
}

// TestTheIngestionWouldPanicOnANanosecondTime is the first reason
// ResolveTimeShape refuses that unit. The converter short circuits on
// `from == to` and hands back what json produced, which is a float64, and the
// ingestion then asserts it to an int64 in a goroutine with no recover.
//
// Should this ever come back as an int64, the refusal in ResolveTimeShape is
// obsolete and this test says so by failing.
func TestTheIngestionWouldPanicOnANanosecondTime(t *testing.T) {
	conv, err := converter.New()
	if err != nil {
		t.Fatal(err)
	}
	//what json.Unmarshal into an interface{} produces for a nanosecond epoch
	asJson := float64(time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC).UnixNano())
	out, err := conv.Cast(asJson, characteristics.UnixNanoSeconds, characteristics.UnixNanoSeconds)
	if err != nil {
		t.Fatalf("expected the cast to succeed and return the wrong type, got the error %v", err)
	}
	if _, isInt64 := out.(int64); isInt64 {
		t.Fatalf("the converter now returns an int64 for a nanosecond time; ResolveTimeShape can stop refusing it")
	}

	//and even with a working assertion the value itself no longer survives the
	//trip: a nanosecond epoch is past the point where a float64 is exact
	exact := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC).UnixNano()
	if int64(float64(exact)) == exact {
		t.Errorf("expected a nanosecond epoch to lose precision as a float64, but %v survived", exact)
	}
}

// TestTheIngestionWouldNeverReadAnIsoTime is the second reason. The ingestion
// flattens before it reads the time, and flatten wraps a string in the single
// quotes it needs for the sql literal, so what reaches the converter is not a
// timestamp any more.
func TestTheIngestionWouldNeverReadAnIsoTime(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	conv, err := converter.New()
	if err != nil {
		t.Fatal(err)
	}

	//unquoted, the cast is fine - which is what makes the failure so easy to
	//miss when reading the converter alone
	if _, err = conv.Cast(at.Format(time.RFC3339), characteristics.IsoTimestamp, characteristics.UnixNanoSeconds); err != nil {
		t.Fatalf("expected an unquoted iso timestamp to cast, got %v", err)
	}

	quoted := flattenLikeThePlatform(map[string]interface{}{"time": at.Format(time.RFC3339)})["time"]
	if _, err = conv.Cast(quoted, characteristics.IsoTimestamp, characteristics.UnixNanoSeconds); err == nil {
		t.Fatalf("the ingestion now reads a quoted iso timestamp; ResolveTimeShape can stop refusing it")
	}
}

// TestTheTwoShapesMosesMustNotSendToATimedService is why the live path was
// changed along with the backfill rather than left alone. Both alternatives to
// carrying the time are measured here against the platform's own code.
func TestTheTwoShapesMosesMustNotSendToATimedService(t *testing.T) {
	service := timedService("root.time", valueMember("value"), timeMember("time", characteristics.UnixSeconds))
	root := service.Outputs[0].ContentVariable.Name

	t.Run("a bare value is rejected on every event", func(t *testing.T) {
		//what moses sent before this change: the value filed under the root name
		_, err := msgvalidation.Clean(map[string]interface{}{root: 42.5}, service)
		if err == nil {
			t.Fatal("expected the platform to reject a bare value sent to a record root")
		}
		if !strings.Contains(err.Error(), "unexpected type") {
			t.Errorf("expected a type complaint, got %v", err)
		}
	})

	t.Run("an object without the time panics the ingestion", func(t *testing.T) {
		cleaned, err := msgvalidation.Clean(
			map[string]interface{}{root: map[string]interface{}{"value": 42.5}}, service)
		if err != nil {
			t.Fatalf("expected the platform to accept the object, got %v", err)
		}
		//the missing member is defaulted rather than left out, so the ingestion
		//finds an entry at the time path and reads it
		timeValue, found := flattenLikeThePlatform(cleaned)[root+".time"]
		if !found {
			t.Fatal("expected the defaulted time member to reach the ingestion")
		}
		if timeValue != nil {
			t.Fatalf("expected the time to be defaulted to null, got %#v", timeValue)
		}
		conv, err := converter.New()
		if err != nil {
			t.Fatal(err)
		}
		cast, err := conv.Cast(timeValue, characteristics.UnixSeconds, characteristics.UnixNanoSeconds)
		if err != nil {
			t.Fatalf("expected the cast to pass the null through, got %v", err)
		}
		//and this is the assertion the ingestion makes, unguarded, in a
		//goroutine with no recover
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the ingestion no longer panics on a null time; the live path could stop carrying one")
				}
			}()
			_ = cast.(int64)
		}()
	})
}

// TestTheMessageMosesSendsSurvivesTheMessageCleaning: the payload names only the
// value and the time, and the platform defaults everything else. A shape that
// tripped the cleaning would be rejected per event, with a notification to the
// device's owner each time.
func TestTheMessageMosesSendsSurvivesTheMessageCleaning(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	service := timedService("root.time",
		valueMember("value"),
		timeMember("time", characteristics.UnixSeconds),
		models.ContentVariable{Name: "quality", Type: models.String})
	shape, err := ResolveTimeShape(service)
	if err != nil {
		t.Fatalf("expected the service to be usable, got %v", err)
	}
	if _, err = ingest(t, service, shape, 42.5, at); err != nil {
		t.Fatalf("the platform could not ingest a payload that leaves a member out: %v", err)
	}
}
