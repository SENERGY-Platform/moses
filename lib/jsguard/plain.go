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

package jsguard

import (
	"errors"
	"fmt"
	"reflect"
)

// MaxStateDepth bounds how deeply a stored state value nests; real scripts store
// flat numbers and strings.
const MaxStateDepth = 32

// MaxStateNodes bounds how many values one stored state value may hold, counting
// every visit: a value sharing subtrees is walked once per path, so this also
// caps the work such a value can cause. Real scripts store single scalars.
const MaxStateNodes = 10000

// ErrStateTooLarge is what a value past MaxStateNodes gets.
var ErrStateTooLarge = fmt.Errorf("the value holds more than %d elements", MaxStateNodes)

// CopyPlainData validates value as plain data - null, booleans, numbers, strings,
// and arrays or plain objects of those - and returns a fresh copy of it in the
// same pass, so what is stored is never a structure a script still holds. A
// function or a Go value bound into a vm would let another channel run code
// inside the vm that stored it, so anything else is refused.
func CopyPlainData(value interface{}) (interface{}, error) {
	budget := MaxStateNodes
	return copyPlain(value, 0, nil, &budget, true)
}

// CheckPlainData is CopyPlainData without the copy.
func CheckPlainData(value interface{}) error {
	_, err := CopyPlainData(value)
	return err
}

// PlainCopy copies a stored value for a reader, so a script gets no live
// reference to a shared Go map. A non-plain leaf, a cycle edge or a level past
// the depth limit is dropped as nil; a value past the node budget is refused
// whole with ErrStateTooLarge, which the caller logs and drops.
func PlainCopy(value interface{}) (interface{}, error) {
	budget := MaxStateNodes
	return copyPlain(value, 0, nil, &budget, false)
}

var errNotPlain = errors.New("not plain data")

// copyPlain is the one walk behind all three. strict refuses what lenient drops;
// the node budget is refused in both, since a lenient walk would still do the
// work.
func copyPlain(value interface{}, depth int, path []interface{}, budget *int, strict bool) (interface{}, error) {
	*budget--
	if *budget < 0 {
		return nil, ErrStateTooLarge
	}
	if depth > MaxStateDepth {
		if strict {
			return nil, fmt.Errorf("the value nests deeper than %d levels", MaxStateDepth)
		}
		return nil, nil
	}
	switch v := value.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return value, nil
	case []interface{}:
		if onPath(path, value) {
			return refuseOrDrop(strict, "the value refers to itself")
		}
		path = append(path, value)
		out := make([]interface{}, len(v))
		for i := range v {
			element, err := copyPlain(v[i], depth+1, path, budget, strict)
			if err != nil {
				return nil, err
			}
			out[i] = element
		}
		return out, nil
	case map[string]interface{}:
		if onPath(path, value) {
			return refuseOrDrop(strict, "the value refers to itself")
		}
		path = append(path, value)
		out := make(map[string]interface{}, len(v))
		for key, element := range v {
			copied, err := copyPlain(element, depth+1, path, budget, strict)
			if err != nil {
				return nil, err
			}
			out[key] = copied
		}
		return out, nil
	}
	if strict {
		return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be stored, not %T", value)
	}
	return nil, nil
}

func refuseOrDrop(strict bool, reason string) (interface{}, error) {
	if strict {
		return nil, fmt.Errorf("%s: %w", reason, errNotPlain)
	}
	return nil, nil
}

// onPath reports whether the container is already on the path from the root,
// which is what a cycle is; the path is short because the depth limit bounds it.
func onPath(path []interface{}, container interface{}) bool {
	p := reflect.ValueOf(container).Pointer()
	for _, ancestor := range path {
		if reflect.ValueOf(ancestor).Pointer() == p {
			return true
		}
	}
	return false
}
