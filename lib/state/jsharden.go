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

package state

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/robertkrimen/otto"
)

// maxScriptCallDepth bounds nested calls in one run. otto evaluates on the Go
// stack, so even plain recursion overflows it (~2200 calls under 8 MB); 1000
// levels stay within a few MB, and legacy scripts do not recurse.
const maxScriptCallDepth = 1000

// hardeningSource replaces otto's JSON.stringify, the one builtin that recurses on the
// Go stack past the call limit, with a JavaScript version whose recursion the limit bounds.
const hardeningSource = `(function (guardRegExp) {
	var global = (function () { return this; })();
	var defineProperty = Object.defineProperty, classOf = Object.prototype.toString;
	var isArrayFn = Array.isArray, keysOf = Object.keys;
	function hide(target, key, value) {
		defineProperty(target, key, {value: value, writable: true, enumerable: false, configurable: true});
	}
	function unavailable(what) {
		return function () { throw new TypeError(what + ' is not available in MOSES scripts'); };
	}

	hide(global, 'eval', unavailable('eval'));
	var blocked = unavailable('the Function constructor');
	blocked.prototype = Function.prototype;
	hide(Function.prototype, 'constructor', blocked);
	hide(global, 'Function', blocked);

	var NativeRegExp = RegExp;
	function isRegExp(value) { return classOf.call(value) === '[object RegExp]'; }
	function patternOf(value) {
		var pattern = value === undefined ? '' : String(value);
		var problem = guardRegExp(pattern);
		if (problem) throw new RangeError(problem);
		return pattern;
	}
	var GuardedRegExp = function RegExp(pattern, flags) {
		return new NativeRegExp(isRegExp(pattern) ? pattern : patternOf(pattern), flags);
	};
	GuardedRegExp.prototype = NativeRegExp.prototype;
	hide(NativeRegExp.prototype, 'constructor', GuardedRegExp);
	hide(global, 'RegExp', GuardedRegExp);

	var nativeMatch = String.prototype.match, nativeSearch = String.prototype.search;
	hide(String.prototype, 'match', function match(regexp) {
		return nativeMatch.call(this, isRegExp(regexp) ? regexp : patternOf(regexp));
	});
	hide(String.prototype, 'search', function search(regexp) {
		return nativeSearch.call(this, isRegExp(regexp) ? regexp : patternOf(regexp));
	});

	function toLength(value) {
		var n = Number(value);
		if (isNaN(n) || n <= 0) return 0;
		if (n > 9007199254740991) return 9007199254740991;
		return Math.floor(n);
	}
	var nativeStringify = JSON.stringify;
	hide(JSON, 'stringify', function stringify(value, replacer, space) {
		var replacerFn = (typeof replacer === 'function') ? replacer : undefined;
		var propertyList;
		if (replacerFn === undefined && isArrayFn(replacer)) {
			propertyList = [];
			var chosen = {}, rlen = toLength(replacer.length);
			for (var i = 0; i < rlen; i++) {
				var rv = replacer[i], item;
				if (typeof rv === 'string') item = rv;
				else if (typeof rv === 'number') item = String(rv);
				else if (rv !== null && typeof rv === 'object') {
					var rc = classOf.call(rv);
					if (rc === '[object String]' || rc === '[object Number]') item = String(rv);
				}
				if (item !== undefined && chosen[item] !== true) { chosen[item] = true; propertyList.push(item); }
			}
		}
		var gap = '', sp = space;
		if (sp !== null && typeof sp === 'object') {
			var sc = classOf.call(sp);
			if (sc === '[object Number]') sp = Number(sp);
			else if (sc === '[object String]') sp = String(sp);
		}
		if (typeof sp === 'number') { var n = Math.min(10, Math.floor(sp)); if (n > 0) gap = Array(n + 1).join(' '); }
		else if (typeof sp === 'string') gap = sp.length > 10 ? sp.substring(0, 10) : sp;

		var stack = [], indent = '';
		function str(key, holder) {
			var v = holder[key];
			if (v !== null && typeof v === 'object' && typeof v.toJSON === 'function') v = v.toJSON(key);
			if (replacerFn !== undefined) v = replacerFn.call(holder, key, v);
			if (v === null) return 'null';
			if (typeof v === 'object') {
				var c = classOf.call(v);
				if (c === '[object Number]') { var nv = Number(v); return isFinite(nv) ? nativeStringify(nv) : 'null'; }
				if (c === '[object String]') return nativeStringify(String(v));
				if (c === '[object Boolean]') return v.valueOf() ? 'true' : 'false';
				for (var s = 0; s < stack.length; s++) if (stack[s] === v) throw new TypeError('Converting circular structure to JSON');
				stack.push(v);
				var out;
				if (isArrayFn(v)) out = ja(v);
				else if (typeof v === 'function') out = undefined;
				else out = jo(v);
				stack.pop();
				return out;
			}
			if (typeof v === 'boolean') return v ? 'true' : 'false';
			if (typeof v === 'string') return nativeStringify(v);
			if (typeof v === 'number') return isFinite(v) ? nativeStringify(v) : 'null';
			return undefined;
		}
		function ja(array) {
			var len = toLength(array.length);
			if (len === 0) return '[]';
			var stepback = indent; indent += gap;
			var parts = [];
			for (var i = 0; i < len; i++) { var s = str(String(i), array); parts.push(s === undefined ? 'null' : s); }
			var out = gap === '' ? '[' + parts.join(',') + ']' : '[\n' + indent + parts.join(',\n' + indent) + '\n' + stepback + ']';
			indent = stepback;
			return out;
		}
		function jo(object) {
			var stepback = indent; indent += gap;
			//otto's native stringify marshals through Go's json, which sorts keys,
			//including a replacer array's; the sort keeps this output-identical to it
			var keys = (propertyList !== undefined ? propertyList.slice() : keysOf(object)).sort(), parts = [];
			for (var i = 0; i < keys.length; i++) {
				var s = str(keys[i], object);
				if (s !== undefined) parts.push(nativeStringify(keys[i]) + (gap === '' ? ':' : ': ') + s);
			}
			var out;
			if (parts.length === 0) out = '{}';
			else out = gap === '' ? '{' + parts.join(',') + '}' : '{\n' + indent + parts.join(',\n' + indent) + '\n' + stepback + '}';
			indent = stepback;
			return out;
		}
		var root = {};
		root[''] = value;
		return str('', root);
	});
})`

// hardenVM prepares a fresh vm for an untrusted script; it must run before the
// script and before anything else is bound.
func hardenVM(vm *otto.Otto) error {
	vm.SetStackDepthLimit(maxScriptCallDepth)
	installer, err := vm.Run(hardeningSource)
	if err != nil {
		return err
	}
	if !installer.IsFunction() {
		return errors.New("the script hardening did not evaluate to a function")
	}
	_, err = installer.Call(otto.NullValue(), func(pattern string) string {
		if err := jsguard.RegexpTooComplex(pattern); err != nil {
			return err.Error()
		}
		return ""
	})
	return err
}

// convertOttoValue turns a script value into bounded plain Go data in one pass that
// reads every property once and never exports a composite, so a getter cannot change the value mid-check.
func convertOttoValue(value otto.Value, maxDepth int, maxNodes int) (interface{}, error) {
	nodes := 0
	bytes := 0
	var conv func(v otto.Value, depth int) (interface{}, error)
	conv = func(v otto.Value, depth int) (interface{}, error) {
		nodes++
		if nodes > maxNodes {
			return nil, fmt.Errorf("the value holds more than %d elements", maxNodes)
		}
		if depth >= maxDepth {
			return nil, fmt.Errorf("the value nests deeper than the %d level limit", maxDepth-1)
		}
		//a missing sparse element, null and undefined all store as nil
		if !v.IsDefined() || v.IsNull() {
			return nil, nil
		}
		if !v.IsObject() {
			exported, err := v.Export()
			if err != nil {
				return nil, err
			}
			switch x := exported.(type) {
			case nil:
				return nil, nil
			case string:
				if bytes += len(x); bytes > maxScriptValueBytes {
					return nil, fmt.Errorf("the value holds more than %d bytes of strings", maxScriptValueBytes)
				}
				return x, nil
			case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
				return x, nil
			default:
				return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored, not %T", exported)
			}
		}
		obj := v.Object()
		switch obj.Class() {
		case "Array":
			length := 0
			if l, err := obj.Get("length"); err == nil {
				if n, err := l.ToInteger(); err == nil {
					length = int(n)
				}
			}
			//grown by append, never preallocated from the untrusted length
			var out []interface{}
			for i := 0; i < length; i++ {
				element, err := obj.Get(strconv.Itoa(i))
				if err != nil {
					return nil, err
				}
				copied, err := conv(element, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, copied)
			}
			if out == nil {
				out = []interface{}{}
			}
			return out, nil
		case "Function":
			return nil, fmt.Errorf("only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored")
		default:
			//every other object (plain, boxed primitive, arguments, Date, Error) is
			//read as a map of its own enumerable properties, as otto's Export did
			out := map[string]interface{}{}
			for _, key := range obj.Keys() {
				//keys count toward the budget too: many objects sharing one huge key
				//would otherwise copy it once per object
				if bytes += len(key); bytes > maxScriptValueBytes {
					return nil, fmt.Errorf("the value holds more than %d bytes of strings", maxScriptValueBytes)
				}
				element, err := obj.Get(key)
				if err != nil {
					return nil, err
				}
				copied, err := conv(element, depth+1)
				if err != nil {
					return nil, err
				}
				out[key] = copied
			}
			return out, nil
		}
	}
	return conv(value, 0)
}

// maxScriptValueBytes bounds the total string bytes one converted value may hold,
// so a getter returning a huge string cannot exhaust memory.
const maxScriptValueBytes = 16 << 20
