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
	"testing"

	"github.com/dop251/goja"
)

// The reimplemented Array join family and JSON.stringify must be output-identical
// to the natives on ordinary values. Native results come from a plain goja, ours
// from a hardened one.
func TestJSBuiltinParity(t *testing.T) {
	exprs := []string{
		// join and its separators, holes, null/undefined
		`[1,2,3].join()`,
		`[1,2,3].join('-')`,
		`[1,null,undefined,2].join('|')`,
		`[[1,2],[3,[4,5]]].join()`,
		`[1,,3].join('x')`,
		`['a','b'].join('')`,
		`String([1,[2,3],4])`,
		"`${[1,2,[3,4]]}`",
		`'' + [1,2,3]`,
		`[3,1,2,[9,8]].sort().join(',')`,
		`[1,2,3].toString()`,
		`[1,[2,3]].toLocaleString()`,
		`[1000.5, 2].toLocaleString()`,
		// flat / flatMap
		`[1,[2,[3,[4]]]].flat().join(',')`,
		`[1,[2,[3,[4]]]].flat(2).join(',')`,
		`[1,[2,[3]]].flat(Infinity).join(',')`,
		`[1,2,3].flatMap(function(x){return [x,x*2]}).join(',')`,
		`[1,[2],3].flatMap(function(x){return x}).join(',')`,
		// JSON.stringify: primitives, nesting, holes, undefined/function
		`JSON.stringify(42)`,
		`JSON.stringify("a\"b\n")`,
		`JSON.stringify(true)`,
		`JSON.stringify(null)`,
		`JSON.stringify([1,2,3])`,
		`JSON.stringify({b:1,a:2,c:[1,2]})`,
		`JSON.stringify([1,undefined,function(){},3])`,
		`JSON.stringify({a:undefined,b:function(){},c:1})`,
		`JSON.stringify([1,,3])`,
		`JSON.stringify({a:{b:{c:1}}})`,
		`JSON.stringify(1.5e21)`,
		`JSON.stringify(-0)`,
		`JSON.stringify(NaN)`,
		`JSON.stringify([NaN, Infinity])`,
		// toJSON
		`JSON.stringify({toJSON:function(){return {x:1}}})`,
		`JSON.stringify({d:{toJSON:function(){return 'X'}}})`,
		// replacer function and array
		`JSON.stringify({a:1,b:2,c:3}, function(k,v){ return k==='b'?undefined:v })`,
		`JSON.stringify({a:1,b:2,c:3}, ['a','c'])`,
		`JSON.stringify([1,2,3], function(k,v){ return typeof v==='number'?v*2:v })`,
		// space
		`JSON.stringify({a:1,b:[2,3]}, null, 2)`,
		`JSON.stringify({a:1,b:[2,3]}, null, '\t')`,
		`JSON.stringify({a:1,b:[2,3]}, null, 20)`,
		`JSON.stringify([], null, 2)`,
		`JSON.stringify({}, null, 2)`,
		// wrapper objects
		`JSON.stringify(new Number(3))`,
		`JSON.stringify(new String("x"))`,
		`JSON.stringify([new Boolean(false), Object(2), Object("s")])`,
		`JSON.stringify({a:new String("x")}, null, 1)`,
		// a spoofed toStringTag does not make a plain object a boxed primitive
		`(function(){ var o={a:1}; o[Symbol.toStringTag]='Number'; return JSON.stringify(o) })()`,
		`(function(){ var o={a:1}; o[Symbol.toStringTag]='String'; return JSON.stringify([o]) })()`,
		// replacer arrays: duplicates, numbers and boxed keys
		`JSON.stringify({b:1,a:2}, ['b','a','b'])`,
		`JSON.stringify({1:1,a:2}, [1])`,
		`JSON.stringify({a:1,b:2}, [new String('b')])`,
		// generic receivers and separators
		`Array.prototype.join.call({length:2, 0:'a', 1:'b'}, '+')`,
		`Array.prototype.join.call('abc')`,
		`[1,2].join(undefined)`,
		`[1,2].join(null)`,
		`Array.prototype.toString.call({join:function(){return 'J'}})`,
		`Array.prototype.toString.call({})`,
		`[null, undefined].toLocaleString()`,
		`String([[], [[]], [,]])`,
		`String(new Int8Array([1,-2]))`,
		`new Float64Array([1.5, 2]).toString()`,
	}
	native := goja.New()
	hardened := goja.New()
	if err := hardenVM(hardened); err != nil {
		t.Fatal(err)
	}
	for _, expr := range exprs {
		want, err := native.RunString(expr)
		if err != nil {
			t.Fatalf("native %q errored: %v", expr, err)
		}
		got, err := hardened.RunString(expr)
		if err != nil {
			t.Fatalf("hardened %q errored: %v", expr, err)
		}
		if want.String() != got.String() {
			t.Errorf("%q:\n native %q\n ours   %q", expr, want.String(), got.String())
		}
	}
}

// Where the native throws, ours throws the same error type: a symbol element, a
// null or undefined receiver and a BigInt, boxed or not, in stringify.
func TestJSBuiltinParityOnThrows(t *testing.T) {
	exprs := []string{
		`[Symbol()].join()`,
		`String([1, [Symbol('s')]])`,
		`Array.prototype.join.call(null)`,
		`Array.prototype.join.call(undefined, ',')`,
		`Array.prototype.toString.call(undefined)`,
		`Array.prototype.toLocaleString.call(null)`,
		`Array.prototype.flat.call(null)`,
		`Array.prototype.flatMap.call(undefined, function(x){return x})`,
		`JSON.stringify(1n)`,
		`JSON.stringify({a: 1n})`,
		`JSON.stringify([Object(1n)])`,
	}
	native := goja.New()
	hardened := goja.New()
	if err := hardenVM(hardened); err != nil {
		t.Fatal(err)
	}
	errorName := func(vm *goja.Runtime, expr string) string {
		v, err := vm.RunString("(function(){ try { " + expr + "; return 'no throw' } catch (e) { return e && e.name } })()")
		if err != nil {
			return "uncatchable: " + err.Error()
		}
		return v.String()
	}
	for _, expr := range exprs {
		want := errorName(native, expr)
		got := errorName(hardened, expr)
		if want == "no throw" {
			t.Fatalf("native %q did not throw, the case is wrong", expr)
		}
		if want != got {
			t.Errorf("%q: native throws %q, ours %q", expr, want, got)
		}
	}
}

// A value deeper than the call limit, however built, ends a run with the call
// limit rather than a fatal Go stack overflow, whichever path reaches it - a
// getter, a Proxy without traps or a toJSON returning a deep value included. The
// test process is alive to make the assertion, which is the proof it did not fatal.
func TestGuardedBuiltinsHitTheCallLimitNotAFatal(t *testing.T) {
	build := "var a = []; for (var i = 0; i < 1000000; i++) a = [a];\n"
	deepViaGetter := "var g = {}; Object.defineProperty(g, 0, {enumerable:true, get:function(){ return a; }}); g.length = 1;\n"
	cases := map[string]string{
		"join":            build + "a.join();",
		"toString":        build + "a.toString();",
		"String":          build + "String(a);",
		"template":        build + "`${a}`;",
		"plus":            build + "a + '';",
		"toLocaleString":  build + "a.toLocaleString();",
		"flat":            build + "a.flat(Infinity);",
		"sort":            build + "[a, a].sort();",
		"stringify":       build + "JSON.stringify(a);",
		"stringifyToJSON": build + "JSON.stringify({toJSON:function(){ return a; }});",
		"stringifyGetter": build + deepViaGetter + "JSON.stringify(g);",
		"joinProxy":       build + "var p=a; for(var j=0;j<50;j++) p=new Proxy(p,{}); [p].join();",
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			vm := goja.New()
			vm.SetMaxCallStackSize(maxScriptCallDepth)
			if err := hardenVM(vm); err != nil {
				t.Fatal(err)
			}
			_, err := vm.RunString(code)
			if err == nil {
				t.Fatalf("%s did not fail on a value past the call limit", name)
			}
		})
	}
}

// convertScriptValue reads each property once and bounds depth, node count and
// string bytes, so a getter or Proxy cannot answer shallow-then-deep and a huge
// string is refused; a getter is invoked exactly once.
func TestConvertScriptValueIsBoundedAndReadsOnce(t *testing.T) {
	vm := goja.New()
	deep, err := vm.RunString("(function(){ var a={}; for(var i=0;i<5000;i++) a={n:a}; return a; })()")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertScriptValue(deep, jsguard_MaxStateDepth1(), 10000); err == nil {
		t.Fatal("a deep value must be refused")
	}
	huge, err := vm.RunString("({ get x(){ return 'y'.repeat(20000000); } })")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertScriptValue(huge, 1000, 1000000); err == nil {
		t.Fatal("a huge string from a getter must be refused")
	}
	//a sparse array claiming a huge length must be refused by the node budget,
	//not preallocated into an out-of-memory slice
	sparse, err := vm.RunString("var s = []; s.length = 1000000000; s;")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertScriptValue(sparse, 1000, 10000); err == nil {
		t.Fatal("a sparse array with a huge length must be refused")
	}
	if _, err := vm.RunString("var reads = 0;"); err != nil {
		t.Fatal(err)
	}
	shallow, err := vm.RunString("({ get p(){ reads++; return 5; } })")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convertScriptValue(shallow, 1000, 1000000); err != nil {
		t.Fatalf("a shallow value must convert: %v", err)
	}
	if got := vm.Get("reads").ToInteger(); got != 1 {
		t.Fatalf("the getter was read %d times, want 1", got)
	}
}

func jsguard_MaxStateDepth1() int { return 33 }

// No property reachable from the global object may still hold a native that the
// hardening replaced: %TypedArray%.prototype.toString is the same object as the
// original Array.prototype.toString and would bring its native recursion back.
func TestNoAliasOfAReplacedNativeSurvives(t *testing.T) {
	vm := goja.New()
	originals, err := vm.RunString(`[Array.prototype.join, Array.prototype.toString, Array.prototype.toLocaleString,
		Array.prototype.flat, Array.prototype.flatMap, JSON.stringify, JSON.parse, eval, Function, RegExp,
		RegExp.prototype.compile, RegExp.prototype[Symbol.split], RegExp.prototype[Symbol.matchAll],
		String.prototype.match, String.prototype.matchAll, String.prototype.search]`)
	if err != nil {
		t.Fatal(err)
	}
	if err := hardenVM(vm); err != nil {
		t.Fatal(err)
	}
	walk, err := vm.RunString(`(function (originals) {
		var found = [], seen = new Set(), queue = [[globalThis, 'globalThis']];
		while (queue.length > 0) {
			var entry = queue.pop(), object = entry[0], path = entry[1];
			if (seen.has(object)) continue;
			seen.add(object);
			var proto = Object.getPrototypeOf(object);
			if (proto !== null) queue.push([proto, path + '.__proto__']);
			Reflect.ownKeys(object).forEach(function (key) {
				var d = Object.getOwnPropertyDescriptor(object, key), name = path + '.' + String(key);
				[d.value, d.get, d.set].forEach(function (v) {
					if (originals.indexOf(v) >= 0) found.push(name);
					if (v !== null && (typeof v === 'object' || typeof v === 'function')) queue.push([v, name]);
				});
			});
		}
		return found.join(' ');
	})`)
	if err != nil {
		t.Fatal(err)
	}
	fn, _ := goja.AssertFunction(walk)
	found, err := fn(goja.Undefined(), originals)
	if err != nil {
		t.Fatal(err)
	}
	if found.String() != "" {
		t.Fatalf("a replaced native is still reachable at %s", found.String())
	}
	//the alias that used to overflow natively now ends with the call limit
	if _, err := vm.RunString(`var o = {}; o.join = Object.getPrototypeOf(Int8Array.prototype).toString; o.join();`); err == nil {
		t.Fatal("the self-joining object must end the run")
	}
}
