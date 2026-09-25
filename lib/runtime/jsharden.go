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
