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
// "senergy/time_path"` and `timeVal, ok := m[attr.Value]` over
// `m := flatten(envelope.Value)`.
const TimePathAttribute = "senergy/time_path"

// TimeUnit is how the platform reads the number at the time path. Only these
// two exist here, and that is not a simplification - see ResolveTimeShape.
type TimeUnit int

const (
	TimeUnitSeconds TimeUnit = iota
	TimeUnitMilliseconds
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

	TimeUnit TimeUnit
}

// ErrNoTimePath is the ordinary case: the service does not declare a time path,
// so the platform stamps its events with the arrival time and the service can
// only carry live data.
var ErrNoTimePath = errors.New("the service carries no " + TimePathAttribute + " attribute, so the platform stamps its events with the arrival time")

// The characteristics the ingestion knows for a time. Taken from the converter
// rather than copied, so a renamed or re-issued id cannot drift apart from what
// the ingestion actually casts with.
var (
	timeUnitsByCharacteristic = map[string]TimeUnit{
		characteristics.UnixSeconds:      TimeUnitSeconds,
		characteristics.UnixMilliSeconds: TimeUnitMilliseconds,
	}
)

// ResolveTimeShape reports how a service wants an event that carries its own
// timestamp, or why it cannot take one.
//
// Every rejection below is a condition under which the platform's ingestion
// would either drop the row or take the connector process down with it, so none
// of them is cosmetic. The two that are not obvious:
//
//   - A nanosecond time characteristic is refused. The ingestion casts the
//     value to UnixNanoSeconds and then asserts `timeVal.(int64)`; for a value
//     that is already nanoseconds the converter short circuits on `from == to`
//     and hands back what the json marshaller produced, which is a float64, so
//     the assertion panics in a goroutine that has no recover. A nanosecond
//     epoch also does not fit into a float64 without loss - 1.8e18 is far past
//     the 9.0e15 where float64 stops being exact on integers - so even a
//     working assertion would round the timestamp.
//   - An iso timestamp characteristic is refused. The ingestion flattens the
//     message before it reads the time, and flatten wraps every string in the
//     single quotes it needs for the sql literal, so the value the converter
//     receives is `'2026-01-01T00:00:00Z'` and time.Parse rejects it. That row
//     is never written.
//
// Both were read from platform-connector-lib v0.0.0-20260826082643 and the
// converter it pins; docs/backfill.md carries the reasoning for a reader who
// has to revisit it after a dependency bump.
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
	unit, known := timeUnitsByCharacteristic[timeVariable.CharacteristicId]
	if !known {
		return TimeShape{}, unsupportedTimeCharacteristic(timeVariable.CharacteristicId)
	}
	if !isNumeric(timeVariable.Type) {
		return TimeShape{}, fmt.Errorf("the time variable is declared as %q; a unix time has to be a number", string(timeVariable.Type))
	}

	valuePath, valueVariable, found := findValueVariable(root, nil, timePath)
	if !found {
		return TimeShape{}, errors.New("no content variable beside the time carries a characteristic, so there is no value to publish")
	}
	if !isNumeric(valueVariable.Type) {
		return TimeShape{}, fmt.Errorf("the value variable %q is declared as %q; a simulated channel publishes a number", strings.Join(valuePath, "."), string(valueVariable.Type))
	}

	return TimeShape{
		RootName:  root.Name,
		ValuePath: valuePath,
		TimePath:  timePath,
		TimeUnit:  unit,
	}, nil
}

func unsupportedTimeCharacteristic(id string) error {
	switch id {
	case "":
		return errors.New("the time variable carries no characteristic, so the platform cannot tell what unit its number is in")
	case characteristics.UnixNanoSeconds:
		return errors.New("the time variable is in unix nanoseconds; the platform's ingestion cannot read that unit without losing precision and crashing, use unix seconds or milliseconds")
	case characteristics.IsoTimestamp:
		return errors.New("the time variable is an iso timestamp; the platform's ingestion quotes strings before it parses them and never accepts one, use unix seconds or milliseconds")
	default:
		return fmt.Errorf("the time variable's characteristic %q is not a unix time the platform can read", id)
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

// timeValue is an integer on purpose. It travels as a json number and comes
// back out of the platform's unmarshaller as a float64, so it has to be a whole
// number small enough to survive that: seconds (1.8e9) and milliseconds
// (1.8e12) both are, nanoseconds (1.8e18) are not, which is the second reason
// ResolveTimeShape refuses that unit.
func (this TimeShape) timeValue(at time.Time) int64 {
	if this.TimeUnit == TimeUnitMilliseconds {
		return at.UnixMilli()
	}
	return at.Unix()
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
