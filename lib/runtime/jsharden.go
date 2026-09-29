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
	"errors"

	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// maxScriptCallDepth bounds nested calls in one run. A call made from inside a
// native function (forEach, sort, replace, a getter, a reviver, a proxy trap)
// recurses on the Go stack; the worst measured costs ~2.8 KB per level, so 1000
// levels stay under 3 MB, and real scripts do not recurse.
const maxScriptCallDepth = 1000

// hardeningSource runs in every vm before the script: code from strings is
// unavailable, and a builtin that parses a string checks it first, on the one
// string conversion the engine then gets, so a toString cannot answer twice.
const hardeningSource = `(function (global, guardJSON, guardRegExp) {
	'use strict';
	var defineProperty = Object.defineProperty, describe = Object.getOwnPropertyDescriptor;
	var prototypeOf = Object.getPrototypeOf, apply = Reflect.apply, construct = Reflect.construct;
	var TypeErrorType = TypeError, RangeErrorType = RangeError, matchSymbol = Symbol.match;
	var isArrayFn = Array.isArray, keysOf = Object.keys;

	function replace(target, key, value) {
		var d = describe(target, key);
		defineProperty(target, key, {value: value, writable: d.writable, enumerable: d.enumerable, configurable: d.configurable});
	}
	function named(name, length, fn) {
		defineProperty(fn, 'name', {value: name, configurable: true});
		defineProperty(fn, 'length', {value: length, configurable: true});
		return fn;
	}
	function unavailable(what) {
		return function () { throw new TypeErrorType(what + ' is not available in MOSES scripts'); };
	}

	replace(global, 'eval', named('eval', 1, unavailable('eval')));
	var constructors = [
		[prototypeOf(function () {}), 'Function'],
		[prototypeOf(function* () {}), 'GeneratorFunction'],
		[prototypeOf(async function () {}), 'AsyncFunction']
	];
	for (var i = 0; i < constructors.length; i++) {
		var proto = constructors[i][0], name = constructors[i][1];
		var blocked = named(name, 1, unavailable('the ' + name + ' constructor'));
		defineProperty(blocked, 'prototype', {value: proto, writable: false, enumerable: false, configurable: false});
		replace(proto, 'constructor', blocked);
		if (name === 'Function') {
			replace(global, 'Function', blocked);
		}
	}

	var parseJSON = JSON.parse;
	replace(JSON, 'parse', named('parse', 2, function (text, reviver) {
		var source = ` + "`${text}`" + `;
		var problem = guardJSON(source);
		if (problem) throw new RangeErrorType(problem);
		return arguments.length > 1 ? apply(parseJSON, JSON, [source, reviver]) : apply(parseJSON, JSON, [source]);
	}));

	var NativeRegExp = global.RegExp, regExpProto = NativeRegExp.prototype;
	var sourceOf = describe(regExpProto, 'source').get;
	// the source getter throws for anything but a real RegExp, proxies included
	function isRegExp(value) {
		if (value === null || (typeof value !== 'object' && typeof value !== 'function') || value === regExpProto) return false;
		try { apply(sourceOf, value, []); return true; } catch (e) { return false; }
	}
	function checked(pattern) {
		var problem = guardRegExp(pattern);
		if (problem) throw new RangeErrorType(problem);
		return pattern;
	}
	function patternOf(value) {
		return checked(value === undefined ? '' : ` + "`${value}`" + `);
	}

	var GuardedRegExp = named('RegExp', 2, function (pattern, flags) {
		var p = pattern, f = flags;
		if (!isRegExp(p)) {
			if (p !== null && (typeof p === 'object' || typeof p === 'function') && p[matchSymbol]) {
				if (f === undefined) f = p.flags;
				p = p.source;
			}
			p = patternOf(p);
		}
		if (new.target === undefined) return NativeRegExp(p, f);
		return construct(NativeRegExp, [p, f], new.target === GuardedRegExp ? NativeRegExp : new.target);
	});
	defineProperty(GuardedRegExp, 'prototype', {value: regExpProto, writable: false, enumerable: false, configurable: false});
	replace(regExpProto, 'constructor', GuardedRegExp);
	replace(global, 'RegExp', GuardedRegExp);

	var compile = regExpProto.compile;
	if (typeof compile === 'function') {
		replace(regExpProto, 'compile', named('compile', 2, function (pattern, flags) {
			return apply(compile, this, [isRegExp(pattern) ? pattern : patternOf(pattern), flags]);
		}));
	}

	// both build a new RegExp from their receiver through the intrinsic constructor
	[Symbol.split, Symbol.matchAll].forEach(function (symbol) {
		var original = regExpProto[symbol];
		if (typeof original !== 'function') return;
		replace(regExpProto, symbol, named(original.name, original.length, function () {
			if (!isRegExp(this)) throw new TypeErrorType(original.name + ' may only be called on a RegExp in MOSES scripts');
			return apply(original, this, arguments);
		}));
	});

	// each compiles a RegExp from an argument that is not one
	['match', 'matchAll', 'search'].forEach(function (method) {
		var original = String.prototype[method];
		if (typeof original !== 'function') return;
		replace(String.prototype, method, named(method, 1, function (regexp) {
			var r = regexp;
			if (r !== undefined && r !== null && !isRegExp(r)) r = patternOf(r);
			return apply(original, this, [r]);
		}));
	});

	// The native join family and JSON.stringify recurse over a nested value on the
	// Go stack; these recurse through JavaScript calls, so the call limit ends a
	// too-deep value safely. A parity test pins them to the natives.
	var arrayProto = Array.prototype, objectToString = Object.prototype.toString, nativeJoin = arrayProto.join;
	//valueOf throws unless the receiver has the matching internal slot, so unlike
	//Object.prototype.toString a spoofed Symbol.toStringTag cannot fool it
	var numberValueOf = Number.prototype.valueOf, stringValueOf = String.prototype.valueOf,
		booleanValueOf = Boolean.prototype.valueOf, bigintValueOf = typeof BigInt !== 'undefined' ? BigInt.prototype.valueOf : null;
	function toLength(value) {
		var n = Number(value);
		if (isNaN(n) || n <= 0) return 0;
		if (n > 9007199254740991) return 9007199254740991;
		return Math.floor(n);
	}
	//ToString: a nested array reaches join again through a JavaScript call, and a
	//Symbol throws
	function toStr(x) {
		if (typeof x === 'symbol') throw new TypeErrorType('Cannot convert a Symbol value to a string');
		return String(x);
	}
	function requireCoercible(receiver, method) {
		if (receiver === undefined || receiver === null) throw new TypeErrorType('Array.prototype.' + method + ' called on null or undefined');
	}

	replace(arrayProto, 'join', named('join', 1, function (separator) {
		requireCoercible(this, 'join');
		var o = Object(this), len = toLength(o.length);
		var sep = separator === undefined ? ',' : toStr(separator);
		//pieces are collected and joined once with the native join; a flat array of
		//strings cannot recurse, so the whole join is linear rather than quadratic.
		var pieces = [];
		for (var k = 0; k < len; k++) {
			var element = o[k];
			pieces.push(element === undefined || element === null ? '' : toStr(element));
		}
		return apply(nativeJoin, pieces, [sep]);
	}));
	var nativeArrayToString = arrayProto.toString;
	replace(arrayProto, 'toString', named('toString', 0, function () {
		requireCoercible(this, 'toString');
		var o = Object(this), join = o.join;
		if (typeof join !== 'function') join = objectToString;
		return apply(join, o, []);
	}));
	//%TypedArray%.prototype.toString is the same native object; a test walks every
	//reachable property for any other alias of a replaced native
	var typedArrayProto = prototypeOf(Int8Array.prototype);
	if (describe(typedArrayProto, 'toString').value === nativeArrayToString) replace(typedArrayProto, 'toString', arrayProto.toString);
	replace(arrayProto, 'toLocaleString', named('toLocaleString', 0, function () {
		requireCoercible(this, 'toLocaleString');
		var o = Object(this), len = toLength(o.length), pieces = [];
		for (var k = 0; k < len; k++) {
			var element = o[k];
			pieces.push(element === undefined || element === null ? '' : toStr(element.toLocaleString()));
		}
		return apply(nativeJoin, pieces, [',']);
	}));

	function flattenInto(target, source, depth) {
		var len = toLength(source.length);
		for (var k = 0; k < len; k++) {
			if (k in source) {
				var element = source[k];
				if (depth > 0 && isArrayFn(element)) flattenInto(target, element, depth - 1);
				else target.push(element);
			}
		}
		return target;
	}
	replace(arrayProto, 'flat', named('flat', 0, function (depth) {
		requireCoercible(this, 'flat');
		var d = depth === undefined ? 1 : Number(depth);
		d = isNaN(d) ? 0 : Math.floor(d);
		return flattenInto([], Object(this), d);
	}));
	replace(arrayProto, 'flatMap', named('flatMap', 1, function (callback, thisArg) {
		requireCoercible(this, 'flatMap');
		var o = Object(this), len = toLength(o.length);
		if (typeof callback !== 'function') throw new TypeErrorType('flatMap callback is not a function');
		var target = [];
		for (var k = 0; k < len; k++) {
			if (k in o) {
				var mapped = apply(callback, thisArg, [o[k], k, o]);
				if (isArrayFn(mapped)) {
					var mlen = toLength(mapped.length);
					for (var j = 0; j < mlen; j++) if (j in mapped) target.push(mapped[j]);
				} else target.push(mapped);
			}
		}
		return target;
	}));

	var nativeStringify = JSON.stringify;
	//the unwrapped primitive, or null without the slot; boxedBigInt reports a
	//BigInt object
	function boxedNumber(v) { try { return { n: numberValueOf.call(v) }; } catch (e) { return null; } }
	function boxedString(v) { try { return { s: stringValueOf.call(v) }; } catch (e) { return null; } }
	function boxedBoolean(v) { try { return { b: booleanValueOf.call(v) }; } catch (e) { return null; } }
	function boxedBigInt(v) { if (bigintValueOf === null) return false; try { bigintValueOf.call(v); return true; } catch (e) { return false; } }
	replace(JSON, 'stringify', named('stringify', 3, function (value, replacer, space) {
		var replacerFn = (typeof replacer === 'function') ? replacer : undefined;
		var propertyList;
		if (replacerFn === undefined && isArrayFn(replacer)) {
			propertyList = [];
			var chosen = Object.create(null), rlen = toLength(replacer.length);
			for (var i = 0; i < rlen; i++) {
				var rv = replacer[i], item;
				if (typeof rv === 'string') item = rv;
				else if (typeof rv === 'number') item = String(rv);
				else if (rv !== null && typeof rv === 'object') {
					var bn = boxedNumber(rv), bs = boxedString(rv);
					if (bs) item = bs.s; else if (bn) item = String(bn.n);
				}
				if (item !== undefined && !chosen[item]) { chosen[item] = true; propertyList.push(item); }
			}
		}
		var gap = '', sp = space;
		if (sp !== null && typeof sp === 'object') {
			var bn = boxedNumber(sp), bs = boxedString(sp);
			if (bn) sp = bn.n; else if (bs) sp = bs.s;
		}
		if (typeof sp === 'number') { var n = Math.min(10, Math.floor(sp)); if (n > 0) gap = ' '.repeat(n); }
		else if (typeof sp === 'string') gap = sp.length > 10 ? sp.substring(0, 10) : sp;

		var stack = [], indent = '';
		function str(key, holder) {
			var v = holder[key];
			if (v !== null && (typeof v === 'object' || typeof v === 'bigint') && typeof v.toJSON === 'function') v = v.toJSON(key);
			if (replacerFn !== undefined) v = apply(replacerFn, holder, [key, v]);
			if (v === null) return 'null';
			if (typeof v === 'object') {
				var b;
				if ((b = boxedNumber(v))) { return isFinite(b.n) ? nativeStringify(b.n) : 'null'; }
				if ((b = boxedString(v))) return nativeStringify(b.s);
				if ((b = boxedBoolean(v))) return b.b ? 'true' : 'false';
				if (boxedBigInt(v)) throw new TypeErrorType('Do not know how to serialize a BigInt');
				for (var s = 0; s < stack.length; s++) if (stack[s] === v) throw new TypeErrorType('Converting circular structure to JSON');
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
			if (typeof v === 'bigint') throw new TypeErrorType('Do not know how to serialize a BigInt');
			return undefined;
		}
		//parts are joined once with the native join, so building each container is
		//linear in its member strings rather than quadratic.
		function ja(array) {
			var len = toLength(array.length);
			if (len === 0) return '[]';
			var stepback = indent; indent += gap;
			var parts = [];
			for (var i = 0; i < len; i++) { var s = str(String(i), array); parts.push(s === undefined ? 'null' : s); }
			var out = gap === '' ? '[' + apply(nativeJoin, parts, [',']) + ']' : '[\n' + indent + apply(nativeJoin, parts, [',\n' + indent]) + '\n' + stepback + ']';
			indent = stepback;
			return out;
		}
		function jo(object) {
			var stepback = indent; indent += gap;
			var keys = propertyList !== undefined ? propertyList : keysOf(object), parts = [];
			for (var i = 0; i < keys.length; i++) {
				var s = str(keys[i], object);
				if (s !== undefined) parts.push(nativeStringify(keys[i]) + (gap === '' ? ':' : ': ') + s);
			}
			var out;
			if (parts.length === 0) out = '{}';
			else out = gap === '' ? '{' + apply(nativeJoin, parts, [',']) + '}' : '{\n' + indent + apply(nativeJoin, parts, [',\n' + indent]) + '\n' + stepback + '}';
			indent = stepback;
			return out;
		}
		var root = {};
		root[''] = value;
		return str('', root);
	}));
})`

var hardeningProgram = mustCompileHardening()

func mustCompileHardening() *goja.Program {
	prg, err := goja.Parse("moses-hardening", hardeningSource, parser.WithDisableSourceMaps)
	if err != nil {
		panic(err)
	}
	program, err := goja.CompileAST(prg, true)
	if err != nil {
		panic(err)
	}
	return program
}

// hardenVM prepares a fresh vm for an untrusted script; it must run before the
// script and before anything else is bound.
func hardenVM(vm *goja.Runtime) error {
	vm.SetParserOptions(parser.WithDisableSourceMaps)
	vm.SetMaxCallStackSize(maxScriptCallDepth)
	installer, err := vm.RunProgram(hardeningProgram)
	if err != nil {
		return err
	}
	install, ok := goja.AssertFunction(installer)
	if !ok {
		return errors.New("the script hardening did not evaluate to a function")
	}
	_, err = install(goja.Undefined(), vm.GlobalObject(),
		vm.ToValue(guardMessage(jsguard.JSONTooComplex)), vm.ToValue(guardMessage(jsguard.RegexpTooComplex)))
	return err
}

// guardMessage adapts a guard to the prelude, which throws on a non-empty answer.
func guardMessage(guard func(string) error) func(string) string {
	return func(text string) string {
		if err := guard(text); err != nil {
			return err.Error()
		}
		return ""
	}
}
