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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// the part that decides the row's timestamp: the connector's marshaller, its
// message cleaning and the converter's cast are exported and are called here
// exactly as the platform calls them.
//
// Two links of that chain are not exported - psql.flatten and
// psql.toNanoseconds - and neither is reachable through psql.Publisher, whose
// only constructor dials postgres. They are mirrored below, and
// TestTheMirroredIngestionInternalsAreStillTheOnesInTheLib fails as soon as the
// file they were read from changes, so a bump cannot silently invalidate them.
//
// That guard is the point. Until platform-connector-lib c8133d0 this file
// pinned the two broken cases by *reproducing* them against the converter and a
// copy of flatten rather than by observing the lib, so when the lib was repaired
// the tests kept passing and said nothing. What they pin now is the repair.

// libraryVersionThePinsWereReadFrom / publisherSourceSha256 record which
// psql/publisher.go the mirrors below were taken from. See
// TestTheMirroredIngestionInternalsAreStillTheOnesInTheLib.
const (
	libraryVersionThePinsWereReadFrom = "v0.0.0-20260827082232-c8133d0f997d"
	publisherSourceSha256             = "0879fda0606d558f64649eed846c6560c95a3eab50f2863aad9d8e8a533b3e74"
)

// flattenLikeThePlatform mirrors `flatten` in platform-connector-lib
// psql/publisher.go. It used to wrap every string in the single quotes the sql
// literal needs, which is what an iso timestamp foundered on; since c8133d0 it
// leaves values as they were decoded and the quoting happens in formatValue,
// after the time has been read.
func flattenLikeThePlatform(m map[string]interface{}) (values map[string]interface{}) {
	values = make(map[string]interface{})
	for k, v := range m {
		switch child := v.(type) {
		case map[string]interface{}:
			nm := flattenLikeThePlatform(child)
			for nk, nv := range nm {
				values[k+"."+nk] = nv
			}
		default:
			values[k] = v
		}
	}
	return values
}

// toNanosecondsLikeThePlatform mirrors `toNanoseconds` in platform-connector-lib
// psql/publisher.go. It replaced the bare `timeVal.(int64)` that used to panic
// in a goroutine with no recover.
func toNanosecondsLikeThePlatform(value interface{}) (nanoseconds int64, err error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case float64:
		if v >= float64(math.MaxInt64) || v < float64(math.MinInt64) {
			return 0, fmt.Errorf("timestamp %v is out of range for unix nanoseconds", v)
		}
		return int64(v), nil
	case float32:
		return toNanosecondsLikeThePlatform(float64(v))
	case json.Number:
		return v.Int64()
	case string:
		return strconv.ParseInt(v, 10, 64)
	default:
		return 0, fmt.Errorf("unable to interpret %T as unix nanoseconds", value)
	}
}

// ingest runs one reading through the platform's own code, from the bytes moses
// puts on the wire to the instant the ingestion stamps the row with. It mirrors
// handleDeviceEvent -> unmarshalMsg -> CleanMsg -> psql.Publish/getTimeString.
func ingest(t *testing.T, service models.Service, shape TimeShape, value float64, at time.Time) (time.Time, error) {
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

	//getTimeString: flatten, look the time up by its full dotted path, cast it,
	//then interpret whatever type came back
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
	cast, err := conv.Cast(timeValue, timeVariable.CharacteristicId, characteristics.UnixNanoSeconds)
	if err != nil {
		return time.Time{}, err
	}
	nanoseconds, err := toNanosecondsLikeThePlatform(cast)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, nanoseconds).UTC(), nil
}

// TestTheMirroredIngestionInternalsAreStillTheOnesInTheLib guards the two copies
// above. They cannot be called in the dependency - both are unexported and
// psql.Publisher is only constructible by dialling postgres - so the next best
// thing a bump can be held to is that the file they were read from is unchanged.
//
// A failure here is not a defect, it is the prompt this file exists for: read
// psql/publisher.go again, check that flatten still leaves strings alone and
// that toNanoseconds still interprets what moses sends, update the mirrors and
// the two constants above.
func TestTheMirroredIngestionInternalsAreStillTheOnesInTheLib(t *testing.T) {
	go_, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("need the go tool to locate the dependency's source: %v", err)
	}
	locate := exec.Command(go_, "list", "-m", "-f", "{{.Dir}}\t{{.Version}}",
		"github.com/SENERGY-Platform/platform-connector-lib")
	//the module is in the cache already, the test binary was linked against it;
	//GOPROXY=off keeps a misconfigured environment from turning this into a
	//network call that hangs
	locate.Env = append(os.Environ(), "GOPROXY=off")
	out, err := locate.Output()
	if err != nil {
		t.Fatalf("could not locate platform-connector-lib: %v", err)
	}
	dir, version, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !ok || dir == "" {
		t.Fatalf("unexpected go list output %q", string(out))
	}
	if version != libraryVersionThePinsWereReadFrom {
		t.Fatalf("platform-connector-lib is %v, the ingestion mirrors in this file were read from %v; "+
			"re-read psql/publisher.go and update them", version, libraryVersionThePinsWereReadFrom)
	}

	source, err := os.ReadFile(filepath.Join(dir, "psql", "publisher.go"))
	if err != nil {
		t.Fatalf("could not read the ingestion source: %v", err)
	}
	sum := sha256.Sum256(source)
	if got := hex.EncodeToString(sum[:]); got != publisherSourceSha256 {
		t.Fatalf("psql/publisher.go hashes to %v, the mirrors in this file were read from %v; "+
			"re-read it and update them", got, publisherSourceSha256)
	}
}

// TestThePlatformStampsTheRowWithTheInstantMosesSends is the contract the whole
// backfill rests on: what comes out at the far end is the instant that went in,
// to the resolution of the declared encoding.
func TestThePlatformStampsTheRowWithTheInstantMosesSends(t *testing.T) {
	//deliberately not a round second, and with digits below the millisecond, so
	//an encoding that silently truncated would be distinguishable from one that
	//did not
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC)

	for name, testCase := range map[string]struct {
		timeVariable models.ContentVariable
		want         time.Time
	}{
		"seconds": {
			timeMember("time", characteristics.UnixSeconds),
			time.Unix(at.Unix(), 0).UTC()},
		"milliseconds": {
			timeMember("time", characteristics.UnixMilliSeconds),
			time.UnixMilli(at.UnixMilli()).UTC()},
		//the one lossy encoding: the json number is decoded into a float64,
		//which around 1.8e18 only represents every 256th integer
		"nanoseconds": {
			timeMember("time", characteristics.UnixNanoSeconds),
			time.Unix(0, int64(float64(at.UnixNano()))).UTC()},
		"nanoseconds as a string": {
			timeTextMember("time", characteristics.UnixNanoSeconds), at},
		"iso timestamp": {
			timeTextMember("time", characteristics.IsoTimestamp), at},
	} {
		t.Run(name, func(t *testing.T) {
			service := timedService("root.time", valueMember("value"), testCase.timeVariable)
			shape, err := ResolveTimeShape(service)
			if err != nil {
				t.Fatalf("expected the service to be usable, got %v", err)
			}
			got, err := ingest(t, service, shape, 42.5, at)
			if err != nil {
				t.Fatalf("the ingestion could not read the time moses sent: %v", err)
			}
			if !got.Equal(testCase.want) {
				t.Errorf("expected the row to be stamped %v, got %v", testCase.want, got)
			}
		})
	}
}

// TestTheIngestionReadsANanosecondTimeWithinTheFloat64Grid replaces the pin that
// used to say this unit crashes the connector. It now says what the repair costs:
// the value is read, and it is read through a float64, which is where the
// timestamp loses its last digits.
//
// Should the loss ever disappear - a decoder using json.Number, say - the second
// half of this test fails and the note in timeValue and docs/backfill.md can go.
func TestTheIngestionReadsANanosecondTimeWithinTheFloat64Grid(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC)
	service := timedService("root.time", valueMember("value"),
		timeMember("time", characteristics.UnixNanoSeconds))
	shape, err := ResolveTimeShape(service)
	if err != nil {
		t.Fatalf("expected a nanosecond service to be usable, got %v", err)
	}

	//the cast itself still hands back a float64: the converter short circuits on
	//`from == to`, which is why the ingestion needs its own type switch
	conv, err := converter.New()
	if err != nil {
		t.Fatal(err)
	}
	cast, err := conv.Cast(float64(at.UnixNano()), characteristics.UnixNanoSeconds, characteristics.UnixNanoSeconds)
	if err != nil {
		t.Fatalf("expected the cast to pass the value through, got %v", err)
	}
	if _, isInt64 := cast.(int64); isInt64 {
		t.Error("the converter now returns an int64 for a nanosecond time; " +
			"the ingestion's type switch is no longer what keeps this working")
	}

	got, err := ingest(t, service, shape, 42.5, at)
	if err != nil {
		t.Fatalf("expected the ingestion to read a nanosecond time, got %v", err)
	}

	//exactly the value the float64 round trip produces, not merely close to it
	if want := time.Unix(0, int64(float64(at.UnixNano()))).UTC(); !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	//1.8e18 lies between 2^60 and 2^61, where float64 steps by 2^8, so the
	//rounding is to the nearest 256 ns and the error is at most half of that
	off := got.UnixNano() - at.UnixNano()
	if off < -128 || off > 128 {
		t.Errorf("expected the rounding to stay within 128 ns, got %d ns", off)
	}
	if off == 0 {
		t.Error("expected this instant to lose precision as a float64; if it no longer does, " +
			"the rounding note in timeValue and docs/backfill.md is obsolete")
	}
}

// TestTheIngestionReadsAnIsoTime replaces the pin that used to say an iso
// timestamp is never read. flatten no longer quotes, so the string reaches
// time.Parse intact - and it is the encoding that carries the instant exactly.
func TestTheIngestionReadsAnIsoTime(t *testing.T) {
	at := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC)
	service := timedService("root.time", valueMember("value"),
		timeTextMember("time", characteristics.IsoTimestamp))
	shape, err := ResolveTimeShape(service)
	if err != nil {
		t.Fatalf("expected an iso service to be usable, got %v", err)
	}

	//the quoting is what used to break this, so pin that it is gone rather than
	//only pinning the outcome
	flat := flattenLikeThePlatform(map[string]interface{}{"time": at.Format(time.RFC3339Nano)})
	if quoted, isString := flat["time"].(string); !isString || strings.HasPrefix(quoted, "'") {
		t.Fatalf("the ingestion quotes strings before it reads the time again; got %#v", flat["time"])
	}

	got, err := ingest(t, service, shape, 42.5, at)
	if err != nil {
		t.Fatalf("expected the ingestion to read an iso timestamp, got %v", err)
	}
	if !got.Equal(at) {
		t.Errorf("expected an iso timestamp to survive to the nanosecond: wanted %v, got %v", at, got)
	}
}

// TestTheTwoShapesMosesMustNotSendToATimedService is why the live path was
// changed along with the backfill rather than left alone. Both alternatives to
// carrying the time are measured here against the platform's own code.
//
// The second one used to take the connector process down. Since c8133d0 it is a
// rejected row and a notification to the device's owners instead - per reading,
// which is still a reason not to send it.
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

	t.Run("an object without the time loses the row", func(t *testing.T) {
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
		//and this is where the ingestion gives up on the row and notifies the
		//device's owners; it used to be an unguarded assertion that panicked
		if _, err := toNanosecondsLikeThePlatform(cast); err == nil {
			t.Error("the ingestion now accepts a null time; the live path could stop carrying one")
		}
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

// TestEveryBackfillableEncodingSurvivesAWholeWindow walks a window the way a job
// does. A single instant can be right by luck - a rounding that happens to land
// on the value, a format that happens to have no fractional part - so this
// checks that the whole series keeps its order and its spacing.
func TestEveryBackfillableEncodingSurvivesAWholeWindow(t *testing.T) {
	start := time.Date(2026, 3, 14, 15, 9, 26, 535_897_123, time.UTC)
	step := 37*time.Second + 421*time.Millisecond

	for name, testCase := range map[string]struct {
		timeVariable models.ContentVariable
		//how far the stamped instant may fall short of the one moses meant, and
		//how far it may overshoot it; a truncating encoding only ever falls
		//short, a rounding one can do either
		shortfall time.Duration
		overshoot time.Duration
	}{
		"seconds": {timeMember("time", characteristics.UnixSeconds),
			time.Second - time.Nanosecond, 0},
		"milliseconds": {timeMember("time", characteristics.UnixMilliSeconds),
			time.Millisecond - time.Nanosecond, 0},
		"nanoseconds":             {timeMember("time", characteristics.UnixNanoSeconds), 128, 128},
		"nanoseconds as a string": {timeTextMember("time", characteristics.UnixNanoSeconds), 0, 0},
		"iso timestamp":           {timeTextMember("time", characteristics.IsoTimestamp), 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			service := timedService("root.time", valueMember("value"), testCase.timeVariable)
			shape, err := ResolveTimeShape(service)
			if err != nil {
				t.Fatalf("expected the service to be usable, got %v", err)
			}
			previous := time.Time{}
			for i := 0; i < 64; i++ {
				at := start.Add(time.Duration(i) * step)
				got, err := ingest(t, service, shape, float64(i), at)
				if err != nil {
					t.Fatalf("reading %d at %v: %v", i, at, err)
				}
				if off := got.Sub(at); off > testCase.overshoot || off < -testCase.shortfall {
					t.Fatalf("reading %d at %v was stamped %v, off by %v", i, at, got, off)
				}
				if !previous.IsZero() && !got.After(previous) {
					t.Fatalf("reading %d at %v was stamped %v, which is not after %v", i, at, got, previous)
				}
				previous = got
			}
		})
	}
}

// TestAnInstantOutsideTheWindowTheApiAllowsIsNotSilentlyWrapped: UnixNano is only
// defined between 1678 and 2262, and the api refuses a window that starts before
// the year 2000 or ends in the future. This pins that the two limits really do
// keep the nanosecond encodings inside their range, so nothing has to guard for
// it further down.
func TestAnInstantOutsideTheWindowTheApiAllowsIsNotSilentlyWrapped(t *testing.T) {
	shape := TimeShape{RootName: "root", ValuePath: []string{"value"},
		TimePath: []string{"time"}, TimeEncoding: TimeAsUnixNanoseconds}

	earliest := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	latest := time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{earliest, latest} {
		nanos, isInt64 := shape.Payload(1, at)["time"].(int64)
		if !isInt64 {
			t.Fatalf("expected an int64 for %v, got %T", at, shape.Payload(1, at)["time"])
		}
		if nanos <= 0 {
			t.Errorf("expected %v to have a positive nanosecond epoch, got %d", at, nanos)
		}
		if back := time.Unix(0, nanos).UTC(); !back.Equal(at) {
			t.Errorf("expected %v to survive UnixNano, got %v", at, back)
		}
	}
}
