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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SENERGY-Platform/converter/lib/converter/characteristics"
	"github.com/SENERGY-Platform/models/go/models"
)

// TimePathAttribute is the service attribute the platform's timescale ingestion
// reads to find the event time inside the payload. Without it a row is stamped
// with the moment it arrived, which is what makes a historical value impossible
// to write.
//
// The value is a dotted path that starts at the name of the output's root
// content variable, because the ingestion looks the value up in the flattened
// message, where every column is prefixed with that root name. Verbatim from
// platform-connector-lib, psql/publisher.go: `var timeAttributeKey =
// "senergy/time_path"`, and `timeVal, ok := m[attr.Value]` in getTimeString over
// the `m := flatten(envelope.Value)` that Publish hands it.
const TimePathAttribute = "senergy/time_path"

// TimeEncoding is how the value at the time path has to be written so that the
// platform's ingestion reads the instant moses meant. It is the characteristic
// of the time variable and, where the characteristic leaves the choice open, the
// declared type of that variable - a nanosecond epoch is read from a json number
// as well as from a string of digits, and the two differ in precision.
type TimeEncoding int

const (
	// TimeAsUnixSeconds is a json number of whole seconds.
	TimeAsUnixSeconds TimeEncoding = iota

	// TimeAsUnixMilliseconds is a json number of whole milliseconds.
	TimeAsUnixMilliseconds

	// TimeAsUnixNanoseconds is a json number of whole nanoseconds. It is the one
	// encoding that does not survive the trip exactly; see timeValue.
	TimeAsUnixNanoseconds

	// TimeAsUnixNanosecondText is the same epoch as a string of digits, which
	// the ingestion reads with strconv.ParseInt and which is therefore exact.
	TimeAsUnixNanosecondText

	// TimeAsIsoTimestamp is an RFC3339 string. Exact to the nanosecond.
	TimeAsIsoTimestamp
)

// TimeShape says where the value and the event time sit inside the payload of
// one platform service, so that a caller can build a message the timescale
// ingestion stamps with a time of the caller's choosing.
//
// The paths are relative to the root content variable, because the root is not
// part of the message a connector sends: the connector unmarshals the protocol
// segment and files the result under the root's name itself.
type TimeShape struct {
	// RootName is the name of the output's root content variable, kept for
	// diagnostics and because it is the prefix of the flattened column names.
	RootName string

	// ValuePath and TimePath name the two leaves below the root. Both are
	// non-empty: a root that is itself the value leaves no room for a time
	// beside it.
	ValuePath []string
	TimePath  []string

	TimeEncoding TimeEncoding
}

// ErrNoTimePath is the ordinary case: the service does not declare a time path,
// so the platform stamps its events with the arrival time and the service can
// only carry live data.
var ErrNoTimePath = errors.New("the service carries no " + TimePathAttribute + " attribute, so the platform stamps its events with the arrival time")

// ResolveTimeShape reports how a service wants an event that carries its own
// timestamp, or why it cannot take one.
//
// All four time characteristics the converter knows are usable. Two of them were
// refused here until platform-connector-lib c8133d0, and the reasons are worth
// keeping because they say what a regression would look like:
//
//   - A nanosecond time used to take the connector process down. The ingestion
//     casts the value to UnixNanoSeconds and, for a value that is already
//     nanoseconds, the converter short circuits on `from == to` and hands back
//     what the json decoder produced - a float64. The ingestion then asserted
//     `timeVal.(int64)`, which panicked in a goroutine with no recover. It now
//     interprets the value with a type switch (psql/publisher.go, toNanoseconds)
//     that covers float64, json.Number and a string of digits.
//   - An iso timestamp used never to be read. The ingestion flattened the
//     message before it looked the time up, and flatten wrapped every string in
//     the single quotes it needs for the sql literal, so time.Parse saw
//     `'2026-01-01T00:00:00Z'` and refused it. flatten now leaves values alone
//     and the quoting happens in formatValue, after the time has been read.
//
// What remains is a declaration check per characteristic: the ingestion reads a
// unix time out of a number and an iso timestamp out of a string, so a variable
// declared as the other type cannot carry one.
//
// Read from platform-connector-lib v0.0.0-20260827082232-c8133d0f997d and the
// converter it pins; docs/backfill.md carries the reasoning, and
// lib/devices/ingestion_test.go pins it against the dependency.
func ResolveTimeShape(service models.Service) (TimeShape, error) {
	//first non-empty attribute wins, which is what the ingestion does
	path := ""
	for _, attribute := range service.Attributes {
		if attribute.Key == TimePathAttribute && attribute.Value != "" {
			path = attribute.Value
			break
		}
	}
	if path == "" {
		return TimeShape{}, ErrNoTimePath
	}

	parts := strings.Split(path, ".")
	if len(parts) < 2 {
		return TimeShape{}, fmt.Errorf("the time path %q names a whole output; the value has to sit beside the time in the same output", path)
	}
	//moses publishes exactly one protocol segment, so it can only fill one
	//output. With a second one the ingestion would read a column moses never
	//wrote, or the same segment would be unmarshalled twice under two names.
	if len(service.Outputs) != 1 {
		return TimeShape{}, fmt.Errorf("the service has %d outputs; moses publishes a single protocol segment and can fill only one", len(service.Outputs))
	}
	output := service.Outputs[0]
	if output.Serialization != models.JSON {
		return TimeShape{}, fmt.Errorf("the output is serialised as %q; moses publishes json", string(output.Serialization))
	}
	root := output.ContentVariable
	if root.Name != parts[0] {
		//the ingestion resolves the characteristic by exactly this comparison
		//and finds nothing, which leaves it casting from an empty characteristic
		return TimeShape{}, fmt.Errorf("the time path starts at %q, but the output's root variable is %q", parts[0], root.Name)
	}

	timePath := parts[1:]
	timeVariable, err := resolveVariable(root, timePath)
	if err != nil {
		return TimeShape{}, fmt.Errorf("the time path %q does not resolve: %w", path, err)
	}
	encoding, err := timeEncodingOf(timeVariable)
	if err != nil {
		return TimeShape{}, err
	}

	valuePath, valueVariable, found := findValueVariable(root, nil, timePath)
	if !found {
		return TimeShape{}, errors.New("no content variable beside the time carries a characteristic, so there is no value to publish")
	}
	if !isNumeric(valueVariable.Type) {
		return TimeShape{}, fmt.Errorf("the value variable %q is declared as %q; a simulated channel publishes a number", strings.Join(valuePath, "."), string(valueVariable.Type))
	}

	return TimeShape{
		RootName:     root.Name,
		ValuePath:    valuePath,
		TimePath:     timePath,
		TimeEncoding: encoding,
	}, nil
}

// timeEncodingOf reads the characteristic of the time variable and, with it, the
// declared type: the ingestion looks a unix time up in a number and an iso
// timestamp in a string, so the pair has to agree before anything is published.
func timeEncodingOf(timeVariable models.ContentVariable) (TimeEncoding, error) {
	switch timeVariable.CharacteristicId {
	case characteristics.UnixSeconds:
		if !isNumeric(timeVariable.Type) {
			return 0, fmt.Errorf("the time variable is declared as %q; a unix time in seconds has to be a number", string(timeVariable.Type))
		}
		return TimeAsUnixSeconds, nil
	case characteristics.UnixMilliSeconds:
		if !isNumeric(timeVariable.Type) {
			return 0, fmt.Errorf("the time variable is declared as %q; a unix time in milliseconds has to be a number", string(timeVariable.Type))
		}
		return TimeAsUnixMilliseconds, nil
	case characteristics.UnixNanoSeconds:
		//both are read: a number through the float64 the json decoder produces,
		//a string through strconv.ParseInt, which is the exact one
		switch {
		case isNumeric(timeVariable.Type):
			return TimeAsUnixNanoseconds, nil
		case timeVariable.Type == models.String:
			return TimeAsUnixNanosecondText, nil
		default:
			return 0, fmt.Errorf("the time variable is declared as %q; a unix time in nanoseconds has to be a number or its digits as a string", string(timeVariable.Type))
		}
	case characteristics.IsoTimestamp:
		if timeVariable.Type != models.String {
			return 0, fmt.Errorf("the time variable is declared as %q; an iso timestamp has to be a string", string(timeVariable.Type))
		}
		return TimeAsIsoTimestamp, nil
	case "":
		return 0, errors.New("the time variable carries no characteristic, so the platform cannot tell what unit its number is in")
	default:
		return 0, fmt.Errorf("the time variable's characteristic %q is not a time the platform can read", timeVariable.CharacteristicId)
	}
}

func isNumeric(t models.Type) bool {
	return t == models.Integer || t == models.Float
}

// resolveVariable walks a path of names below root, refusing every step the
// platform's own message cleaning would choke on.
func resolveVariable(root models.ContentVariable, path []string) (models.ContentVariable, error) {
	current := root
	for _, name := range path {
		if current.Type != models.Structure {
			return current, fmt.Errorf("%q is declared as %q and has no member %q", current.Name, string(current.Type), name)
		}
		if len(current.SubContentVariables) == 0 {
			//the platform's message cleaning indexes SubContentVariables[0]
			//without checking, so this shape takes the connector down
			return current, fmt.Errorf("%q is a structure without members", current.Name)
		}
		if current.SubContentVariables[0].Name == "*" {
			//a map, not a record: a named key would be read as an entry
			return current, fmt.Errorf("%q is a map, not a record with a member %q", current.Name, name)
		}
		next, found := memberOf(current, name)
		if !found {
			return current, fmt.Errorf("%q has no member %q", current.Name, name)
		}
		current = next
	}
	return current, nil
}

func memberOf(variable models.ContentVariable, name string) (models.ContentVariable, bool) {
	for _, sub := range variable.SubContentVariables {
		if sub.Name == name {
			return sub, true
		}
	}
	return models.ContentVariable{}, false
}

// findValueVariable picks the leaf that carries the measured value: the first
// content variable with a characteristic, depth first, skipping the one the
// time path points at. This is the same rule the catalog offers an editor
// (valueOf), minus the time - otherwise a service whose time variable happens
// to come first would publish its value into the time column.
func findValueVariable(variable models.ContentVariable, path []string, timePath []string) ([]string, models.ContentVariable, bool) {
	if len(path) > 0 && samePath(path, timePath) {
		return nil, models.ContentVariable{}, false
	}
	if variable.CharacteristicId != "" && len(path) > 0 {
		return path, variable, true
	}
	if variable.Type != models.Structure || len(variable.SubContentVariables) == 0 {
		return nil, models.ContentVariable{}, false
	}
	if variable.SubContentVariables[0].Name == "*" {
		return nil, models.ContentVariable{}, false
	}
	for _, sub := range variable.SubContentVariables {
		//a fresh slice per branch: appending onto the caller's array would let
		//two branches overwrite each other's names
		below := make([]string, len(path), len(path)+1)
		copy(below, path)
		below = append(below, sub.Name)
		if found, value, ok := findValueVariable(sub, below, timePath); ok {
			return found, value, true
		}
	}
	return nil, models.ContentVariable{}, false
}

func samePath(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Payload builds the message body for one reading: the value at the value path
// and the instant at the time path, both below the root, which is what the
// connector unmarshals the protocol segment into.
//
// Nothing else is filled in. The platform defaults the members a message does
// not mention, so a partial record is the normal shape rather than a gap.
func (this TimeShape) Payload(value float64, at time.Time) map[string]interface{} {
	root := map[string]interface{}{}
	setAt(root, this.ValuePath, value)
	setAt(root, this.TimePath, this.timeValue(at))
	return root
}

// timeValue is the instant in the encoding the service declares.
//
// Three of the four encodings survive the trip exactly. Seconds (1.8e9) and
// milliseconds (1.8e12) are whole numbers well inside the 9.0e15 up to which a
// float64 represents integers exactly, and the two text encodings never meet a
// float64 at all.
//
// TimeAsUnixNanoseconds is the exception, and it is a deliberate one. The value
// travels as a json number, and the connector decodes json numbers into float64
// - its json marshaller calls json.Unmarshal into an interface{} without
// UseNumber, so json.Number is not reachable from here. A float64 carries 53
// significant bits, so around the current epoch of 1.8e18 ns, which lies between
// 2^60 and 2^61, only every 256th integer is representable and the timestamp is
// rounded to the nearest of them. What timescale stores is therefore exactly
// int64(float64(at.UnixNano())): off by at most 128 ns, and by at most 256 ns
// after 2043, when the step doubles again.
//
// That is accepted rather than refused. These rows are training data for
// operator models sampled at seconds or minutes, so 128 ns is far below the
// resolution of anything that reads them, and refusing the unit would make a
// device type unbackfillable over a rounding no consumer can observe. A device
// type that declares its nanosecond time as a string gets the exact value
// instead, because the ingestion parses those digits with strconv.ParseInt.
// docs/backfill.md states the same, for whoever has to weigh it again.
//
// UnixNano is only defined between 1678 and 2262. The api refuses a window that
// starts before the year 2000 or ends in the future, so neither the backfill nor
// the live path can reach that edge; an iso timestamp would not have it at all.
func (this TimeShape) timeValue(at time.Time) interface{} {
	switch this.TimeEncoding {
	case TimeAsUnixMilliseconds:
		return at.UnixMilli()
	case TimeAsUnixNanoseconds:
		return at.UnixNano()
	case TimeAsUnixNanosecondText:
		return strconv.FormatInt(at.UnixNano(), 10)
	case TimeAsIsoTimestamp:
		//UTC so the string is the same wherever moses runs, and RFC3339Nano so
		//the sub-second part is carried; time.Parse reads a fractional second
		//even though the layout the ingestion parses with, time.RFC3339, does
		//not spell one out
		return at.UTC().Format(time.RFC3339Nano)
	default:
		return at.Unix()
	}
}

func setAt(root map[string]interface{}, path []string, value interface{}) {
	current := root
	for _, name := range path[:len(path)-1] {
		next, ok := current[name].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			current[name] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
}
