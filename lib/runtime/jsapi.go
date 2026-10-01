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

package runtime

import (
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/dop251/goja"
)

// The javascript surface is the legacy one: a migrated channel carries its
// script verbatim, so moses.world, room, device and service keep their meaning,
// now against the environment, the asset's zone, the asset and the channel. The
// new names are aliases onto the same maps:
//
//	moses.world   == moses.environment
//	moses.room    == moses.zone
//	moses.device  == moses.asset
//	moses.service == moses.channel
//
// Everything below runs inside a script run with the environment mutex held,
// which is what makes the state maps safe without locking of their own. now is
// the instant of the run the api is built for: a zone value with a time constant
// resolves against it, so a script sees one moment however long it runs.
func (this *Runtime) jsApi(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{}), now time.Time) map[string]interface{} {
	environmentApi := this.jsEnvironmentApi(env, gen, now)
	zoneApi := this.jsZoneApi(env, gen, binding.zoneId, now)
	assetApi := this.jsAssetApi(env, binding.asset.id)
	channelApi := map[string]interface{}{
		"input": input,
		//a goja.Value converted in one bounded pass, never exported; the error makes
		//a call re-entered from a getter throw instead of nesting a conversion
		"send": func(value goja.Value) error {
			//a missing argument, an explicit null and an explicit undefined all
			//mean no reading, where otto aborted the run; nothing is published
			//rather than a null
			if isNoValue(value) {
				util.Logger.Warn("the script handed no value", "environment", env.id, "field", "send")
				return nil
			}
			if !env.sink.Enter() {
				return jsguard.ErrSinkReentry
			}
			defer env.sink.Leave()
			converted, err := convertScriptValue(value, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes)
			if err != nil {
				util.Logger.Warn("the script handed a value that cannot be published, it is dropped",
					attributes.ErrorKey, err, "environment", env.id, "field", "send")
				return nil
			}
			//after the conversion: a getter it ran may have spent the run's time in httpGet
			if env.sink.Expired() {
				return ErrScriptTimeout
			}
			send(jsNumber(converted))
			return nil
		},
	}
	return map[string]interface{}{
		"world":   environmentApi,
		"room":    zoneApi,
		"device":  assetApi,
		"service": channelApi,

		//aliases, deliberately the same maps and not copies
		"environment": environmentApi,
		"zone":        zoneApi,
		"asset":       assetApi,
		"channel":     channelApi,
	}
}

func (this *Runtime) jsEnvironmentApi(env *environment, gen *generation, now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"state": jsContextStateApi(env, gen, now),
		"getRoom": func(zoneId string) map[string]interface{} {
			if _, known := gen.zones[zoneId]; !known {
				util.Logger.Warn("no zone for id found", "environment", env.id, "id", zoneId)
				return map[string]interface{}{}
			}
			return this.jsZoneApi(env, gen, zoneId, now)
		},
	}
}

func (this *Runtime) jsZoneApi(env *environment, gen *generation, zoneId string, now time.Time) map[string]interface{} {
	return map[string]interface{}{
		//a zone value with a time constant is resolved here rather than on a
		//ticker: the mutex is held, so this is the one place it can be exact
		"state": jsStateApi(env, func() map[string]interface{} {
			env.advanceZone(zoneId, now)
			return env.zoneStates(zoneId)
		}),
		"getDevice": func(assetId string) map[string]interface{} {
			asset, known := gen.assets[assetId]
			//the asset has to sit in this zone, exactly like the legacy
			//room.getDevice() only found the devices of that room
			if !known || asset.zoneId != zoneId {
				util.Logger.Warn("no asset for id found in this zone", "environment", env.id, "zone", zoneId, "id", assetId)
				return map[string]interface{}{}
			}
			return this.jsAssetApi(env, assetId)
		},
	}
}

func (this *Runtime) jsAssetApi(env *environment, assetId string) map[string]interface{} {
	return map[string]interface{}{
		"state": jsStateApi(env, func() map[string]interface{} { return env.assetStates(assetId) }),
	}
}

// jsStateApi is the get/set pair of one scope. states() resolves the map late,
// so a scope whose map does not exist yet is created on first use.
//
// get seeds a missing key with 0 and returns it, as the legacy api did: a
// migrated script relies on "state.get() + 1" working on the first tick. The
// seeding is a state change, so it marks the environment dirty; it happens once
// per key, because the next get finds the key. A missing or empty field name is
// refused rather than becoming a key; a number or boolean names the key the
// engines always spelled it as.
func jsStateApi(env *environment, states func() map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		//goja.Values, not interface{}: goja would export (and recurse over) an
		//interface{} argument before the function runs
		"get": func(field goja.Value) interface{} {
			name, ok := jsFieldValue(field)
			if !ok {
				env.warnNoField()
				return 0
			}
			target := states()
			value, ok := target[name]
			if !ok {
				if env.sink.Expired() {
					return 0
				}
				target[name] = 0
				env.dirty = true
				return 0
			}
			//a copy, never the live map: goja would wrap a returned map as a
			//live object, and a script could then write a function straight into
			//the shared state, past the set check
			return readState(env, name, value)
		},
		"set": func(field goja.Value, value goja.Value) error {
			name, ok := jsFieldValue(field)
			if !ok {
				env.warnNoField()
				return nil
			}
			//a missing argument, an explicit null and an explicit undefined all
			//mean no value, where otto aborted the run; the key keeps whatever
			//it holds rather than taking a nil no reader can use
			if isNoValue(value) {
				util.Logger.Warn("the script handed no value", "environment", env.id, "field", name)
				return nil
			}
			if !env.sink.Enter() {
				return jsguard.ErrSinkReentry
			}
			defer env.sink.Leave()
			//converted in one bounded pass that reads each property once and never
			//exports a composite, at the state bounds, so only a fresh copy of plain
			//data within depth and node limits is stored
			converted, err := convertScriptValue(value, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
			if err != nil {
				return fmt.Errorf("state %q: %w", name, err)
			}
			//the plain-data check the old path ran: a Date or BigInt converts to a
			//scalar but is not storable, and is refused as before
			copied, err := jsguard.CopyPlainData(converted)
			if err != nil {
				return fmt.Errorf("state %q: %w", name, err)
			}
			//after the conversion: a getter it ran may have spent the run's time in httpGet
			if env.sink.Expired() {
				return ErrScriptTimeout
			}
			states()[name] = jsNumber(copied)
			env.dirty = true
			return nil
		},
	}
}

// jsField turns a script's field argument into a key. A missing argument, null
// and undefined arrive as nil and name no key, nor does the empty string; a
// number or boolean is spelled out as both engines did when the parameter was a
// string, so a legacy script keyed by an hour keeps its key.
func jsField(field interface{}) (string, bool) {
	switch v := field.(type) {
	case string:
		return v, v != ""
	case int64, int, float64, bool:
		return fmt.Sprint(v), true
	}
	return "", false
}

// jsFieldValue turns a script's field argument into a key without letting goja
// export (and recurse over) it first: a valid field is a scalar, so a value that
// is an object at all is refused, and a scalar is exported once and spelled out
// by jsField. A missing, deep or non-scalar field names no key.
func jsFieldValue(field goja.Value) (string, bool) {
	if isNoValue(field) {
		return "", false
	}
	if _, isObject := field.(*goja.Object); isObject {
		return "", false
	}
	return jsField(field.Export())
}

// isNoValue reports the three script values that mean "nothing was handed in": a
// missing argument arrives as a nil goja.Value, and null and undefined as their
// own singletons.
func isNoValue(value goja.Value) bool {
	return value == nil || goja.IsUndefined(value) || goja.IsNull(value)
}

// convertScriptValue turns a script value into bounded plain Go data in one pass that
// reads every property once and never exports a composite, so a getter cannot change the value mid-check.
func convertScriptValue(value goja.Value, maxDepth int, maxNodes int) (interface{}, error) {
	nodes := 0
	bytes := 0
	var conv func(v goja.Value, depth int, seen map[*goja.Object]bool) (interface{}, error)
	conv = func(v goja.Value, depth int, seen map[*goja.Object]bool) (interface{}, error) {
		nodes++
		if nodes > maxNodes {
			return nil, fmt.Errorf("the value holds more than %d elements", maxNodes)
		}
		if depth >= maxDepth {
			return nil, fmt.Errorf("the value nests deeper than the %d level limit", maxDepth-1)
		}
		//a missing sparse element arrives as a nil Value; null and undefined store
		//as nil, as they always did
		if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
			return nil, nil
		}
		obj, isObject := v.(*goja.Object)
		if !isObject {
			//a primitive: exporting it does not recurse
			switch exported := v.Export().(type) {
			case nil:
				return nil, nil
			case string:
				if bytes += len(exported); bytes > maxScriptValueBytes {
					return nil, fmt.Errorf("the value holds more than %d bytes of strings", maxScriptValueBytes)
				}
				return exported, nil
			case bool, int64, int, float64:
				return exported, nil
			default:
				return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored, not %T", exported)
			}
		}
		if seen[obj] {
			return nil, fmt.Errorf("the value refers to itself")
		}
		seen[obj] = true
		defer delete(seen, obj)
		//distinguish a plain array or object from anything else without exporting:
		//exportType reports the type without walking the value. arguments walks as
		//the array it exported to; a Set shares the array ExportType but has no
		//length and is refused rather than sent empty
		exportType := obj.ExportType()
		if obj.ClassName() == "Arguments" {
			exportType = reflectTypeScriptArray
		}
		switch exportType {
		case reflectTypeScriptArray:
			l := obj.Get("length")
			if l == nil {
				return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored")
			}
			//grown by append, never preallocated from the untrusted length, so a
			//sparse array claiming a billion elements allocates nothing up front
			length := int(l.ToInteger())
			var out []interface{}
			for i := 0; i < length; i++ {
				element, err := conv(obj.Get(strconv.Itoa(i)), depth+1, seen)
				if err != nil {
					return nil, err
				}
				out = append(out, element)
			}
			if out == nil {
				out = []interface{}{}
			}
			return out, nil
		case reflectTypeScriptObject:
			out := map[string]interface{}{}
			for _, key := range obj.Keys() {
				//keys count toward the budget too: many objects sharing one huge key
				//would otherwise copy it once per object
				if bytes += len(key); bytes > maxScriptValueBytes {
					return nil, fmt.Errorf("the value holds more than %d bytes of strings", maxScriptValueBytes)
				}
				copied, err := conv(obj.Get(key), depth+1, seen)
				if err != nil {
					return nil, err
				}
				out[key] = copied
			}
			return out, nil
		default:
			//a boxed Number or Boolean, a Date or a BigInt object exports to one
			//scalar without walking anything, as the old path did; a function,
			//Proxy, Map or typed array is refused rather than exported unbounded
			if exportsToScalar(exportType) {
				return obj.Export(), nil
			}
			return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored")
		}
	}
	return conv(value, 0, map[*goja.Object]bool{})
}

// exportsToScalar reports the ExportTypes whose Export is a single scalar: a
// boxed Number or Boolean, a Date and a BigInt.
func exportsToScalar(t reflect.Type) bool {
	if t == reflectTypeTime || t == reflectTypeBigInt {
		return true
	}
	switch t.Kind() {
	case reflect.Int64, reflect.Float64, reflect.Bool:
		return true
	}
	return false
}

// maxScriptValueBytes bounds the total string bytes one converted value may hold,
// so a getter returning a huge string cannot exhaust memory before the node limit
// is reached.
const maxScriptValueBytes = 16 << 20

// reflectTypeScriptArray and reflectTypeScriptObject are what goja's ExportType
// reports for a plain array and a plain object, used to tell them apart from a
// function, Date, Proxy or typed array without exporting the value.
var (
	reflectTypeScriptArray  = reflect.TypeOf([]interface{}{})
	reflectTypeScriptObject = reflect.TypeOf(map[string]interface{}{})
	reflectTypeTime         = reflect.TypeOf(time.Time{})
	reflectTypeBigInt       = reflect.TypeOf((*big.Int)(nil))
)

// jsNumber normalises a number on its way from a script into Go. A javascript
// engine may export an integral number as int64 and a computed one as float64,
// so without this the same script could leave two types in the state maps for
// the same value; a javascript number is a float64 to begin with, so the
// conversion loses nothing. Everything that is not a number passes through.
func jsNumber(value interface{}) interface{} {
	if number, ok := asFloat(value); ok {
		return number
	}
	return value
}

// jsContextStateApi is the context scope of the script api. It differs from
// every other scope in one thing: a key the document's timeline governs is
// read-only here.
//
// get answers with the declared value of this instant and does not seed the key,
// since seeding would persist a value the document decides. set is dropped with
// one warning per key: a script writing such a key would be overwritten by the
// next read anyway, and a value that looks set and is not is the harder failure
// to find. Every other key behaves exactly as it did.
func jsContextStateApi(env *environment, gen *generation, now time.Time) map[string]interface{} {
	plain := jsStateApi(env, func() map[string]interface{} { return env.contextStates() })
	if gen == nil || gen.timeline == nil {
		return plain
	}
	get := plain["get"].(func(field goja.Value) interface{})
	set := plain["set"].(func(field goja.Value, value goja.Value) error)
	return map[string]interface{}{
		"get": func(field goja.Value) interface{} {
			name, ok := jsFieldValue(field)
			if !ok || !gen.timeline.governsContext(name) {
				//an invalid field falls through to the plain get, which refuses it
				return get(field)
			}
			if value, governed := gen.timeline.effectiveContext(name, now); governed {
				return readState(env, name, value)
			}
			//before the first change the inline value stands, and seeding put it
			//into the state at start; a key that is missing anyway reads as 0
			//without being written
			if value, exists := env.contextStates()[name]; exists {
				return readState(env, name, value)
			}
			return 0
		},
		"set": func(field goja.Value, value goja.Value) error {
			name, ok := jsFieldValue(field)
			if ok && gen.timeline.governsContext(name) {
				env.warnTimelineGoverned(name)
				return nil
			}
			//an invalid field falls through to the plain set, which refuses it
			return set(field, value)
		},
	}
}

// readState hands a stored value to a script as a plain-data copy. A value past
// the node budget, which set refuses and only a corrupted state can hold, is
// logged and read as null.
func readState(env *environment, field string, value interface{}) interface{} {
	copied, err := jsguard.PlainCopy(value)
	if err != nil {
		util.Logger.Warn("a stored state value is too large to read, it reads as null", attributes.ErrorKey, err, "environment", env.id, "field", field)
		return nil
	}
	return copied
}
