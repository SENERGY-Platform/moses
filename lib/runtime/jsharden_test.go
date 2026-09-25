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
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

func hardenedVM(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	if err := hardenVM(vm); err != nil {
		t.Fatalf("hardening failed: %v", err)
	}
	return vm
}

// expectThrow runs code and requires it to throw an error of the given type
// whose message contains fragment.
func expectThrow(t *testing.T, vm *goja.Runtime, code, errorType, fragment string) {
	t.Helper()
	_, err := vm.RunString(code)
	if err == nil {
		t.Errorf("%s: did not throw", code)
		return
	}
	var ex *goja.Exception
	if !asException(err, &ex) {
		t.Errorf("%s: not a script exception: %v", code, err)
		return
	}
	obj := ex.Value().ToObject(vm)
	if got := obj.Get("name").String(); got != errorType {
		t.Errorf("%s: threw %s (%v), want %s", code, got, err, errorType)
	}
	if !strings.Contains(obj.Get("message").String(), fragment) {
		t.Errorf("%s: message %q lacks %q", code, obj.Get("message").String(), fragment)
	}
}

func asException(err error, target **goja.Exception) bool {
	ex, ok := err.(*goja.Exception)
	if ok {
		*target = ex
	}
	return ok
}

func expectTrue(t *testing.T, vm *goja.Runtime, code string) {
	t.Helper()
	v, err := vm.RunString(code)
	if err != nil {
		t.Errorf("%s: %v", code, err)
		return
	}
	if !v.ToBoolean() {
		t.Errorf("%s: false", code)
	}
}

func TestCodeFromStringsIsUnavailable(t *testing.T) {
	vm := hardenedVM(t)
	for _, code := range []string{
		`eval("1")`,
		`(0, eval)("1")`,
		`globalThis.eval("1")`,
		`(function () { var local = 1; return eval("local"); })()`,
		`Function("return 1")()`,
		`new Function("return 1")`,
		`(function () {}).constructor("return 1")`,
		`Object.getPrototypeOf(function () {}).constructor("return 1")`,
		`(() => 1).constructor("return 1")`,
		`(class {}).constructor("return 1")`,
		`Reflect.construct(Function, ["return 1"])`,
		`Function.prototype.constructor.call(null, "return 1")`,
		`Object.getOwnPropertyDescriptor(Function.prototype, "constructor").value("return 1")`,
		`(function* () {}).constructor("yield 1")`,
		`Object.getPrototypeOf(function* () {}).constructor("yield 1")`,
		`(async function () {}).constructor("return 1")`,
		`Object.getPrototypeOf(async function () {}).constructor("return 1")`,
		`(async () => 1).constructor("return 1")`,
	} {
		expectThrow(t, vm, code, "TypeError", "not available in MOSES scripts")
	}
}

func TestFunctionsStillBehaveAsFunctions(t *testing.T) {
	vm := hardenedVM(t)
	expectTrue(t, vm, `(function () {}) instanceof Function`)
	expectTrue(t, vm, `(() => 1) instanceof Function && typeof Math.max === "function"`)
	expectTrue(t, vm, `(function* () {}) instanceof Object.getPrototypeOf(function* () {}).constructor`)
	expectTrue(t, vm, `[3, 1, 2].sort(function (a, b) { return a - b; }).join() === "1,2,3"`)
}

func TestJSONParseIsGuardedAndStillParses(t *testing.T) {
	vm := hardenedVM(t)
	expectTrue(t, vm, `JSON.parse('{"a":[1,{"b":2}]}').a[1].b === 2`)
	expectTrue(t, vm, `JSON.parse('[1,2]', function (k, v) { return typeof v === "number" ? v * 10 : v; })[1] === 20`)
	expectTrue(t, vm, `JSON.parse.name === "parse" && JSON.parse.length === 2`)
	expectThrow(t, vm, `JSON.parse("[".repeat(1001) + "]".repeat(1001))`, "RangeError", "JSON nests deeper")
	expectThrow(t, vm, `JSON.parse("{")`, "SyntaxError", "")
}

func TestRegExpIsGuardedAndStillWorks(t *testing.T) {
	vm := hardenedVM(t)
	expectTrue(t, vm, `new RegExp("a+", "g").test("caab") && RegExp("b").test("b")`)
	expectTrue(t, vm, `/x/ instanceof RegExp && /x/.constructor === RegExp && new RegExp("y") instanceof RegExp`)
	expectTrue(t, vm, `new RegExp(/ab/g).flags === "g" && new RegExp(/ab/, "i").flags === "i"`)
	expectTrue(t, vm, `class R extends RegExp {}; new R("z") instanceof R && new R("z").test("z")`)
	expectTrue(t, vm, `"a,b".split(/,/).length === 2 && "xaax".replace(/a/g, "b") === "xbbx"`)
	expectTrue(t, vm, `"a1b2".match("\\d")[0] === "1" && "abc".search("c") === 2 && [..."a1b2".matchAll(/\d/g)].length === 2`)
	expectTrue(t, vm, `var re = /a/; re.compile("b"); re.test("b")`)
	deep := `"(".repeat(201) + "a" + ")".repeat(201)`
	for _, code := range []string{
		`new RegExp(` + deep + `)`,
		`RegExp(` + deep + `)`,
		`/a/.compile(` + deep + `)`,
		`"a".match(` + deep + `)`,
		`"a".search(` + deep + `)`,
		`"a".matchAll(` + deep + `)`,
		`new RegExp({ source: ` + deep + `, flags: "", [Symbol.match]: true })`,
		`new RegExp({ toString: function () { return ` + deep + `; } })`,
	} {
		expectThrow(t, vm, code, "RangeError", "regular expression nests")
	}
}

// TestRegExpSpeciesPathsNeedARealReceiver: these build a RegExp from their
// receiver through the intrinsic constructor, which a plain object could feed.
func TestRegExpSpeciesPathsNeedARealReceiver(t *testing.T) {
	vm := hardenedVM(t)
	fake := `{ source: "(".repeat(201) + "a" + ")".repeat(201), flags: "g", [Symbol.match]: true }`
	expectThrow(t, vm, `RegExp.prototype[Symbol.split].call(`+fake+`, "x")`, "TypeError", "may only be called on a RegExp")
	expectThrow(t, vm, `RegExp.prototype[Symbol.matchAll].call(`+fake+`, "x")`, "TypeError", "may only be called on a RegExp")
	expectThrow(t, vm, `RegExp.prototype[Symbol.split].call(new Proxy(/a/, {}), "x")`, "TypeError", "may only be called on a RegExp")
}

// TestPatternIsConvertedOnce: a toString that answers the check with one pattern
// and the engine with another must not get the second one compiled.
func TestPatternIsConvertedOnce(t *testing.T) {
	vm := hardenedVM(t)
	expectTrue(t, vm, `var calls = 0;
		var sly = { toString: function () { calls++; return calls === 1 ? "ab" : "(".repeat(5000) + "a" + ")".repeat(5000); } };
		var re = new RegExp(sly);
		re.source === "ab" && calls === 1`)
	expectTrue(t, vm, `var n = 0;
		var sly2 = { toString: function () { n++; return n === 1 ? "[1]" : "[".repeat(5000); } };
		JSON.parse(sly2)[0] === 1 && n === 1`)
}

// TestNestedCallsThroughNativeCodeAreBounded: the limit ends the run even through
// try/catch; the recursion stops at 5000 by itself, so it is harmless without the limit.
func TestNestedCallsThroughNativeCodeAreBounded(t *testing.T) {
	vm := hardenedVM(t)
	_, err := vm.RunString(`var d = 0; function f() { d++; if (d < 5000) try { [1].forEach(f); } catch (e) {} } f()`)
	var overflow *goja.StackOverflowError
	if !errors.As(err, &overflow) {
		t.Fatalf("expected the call depth limit, got %v", err)
	}
	expectTrue(t, hardenedVM(t), `var e = 0; function g() { e++; if (e < 200) g(); } g(); e === 200`)
}

// TestHardenedRunSurvivesDeepInputs runs every known crash input through the
// script path in a child under an 8 MB max stack, where each overflows unhardened.
func TestHardenedRunSurvivesDeepInputs(t *testing.T) {
	attacks := []struct{ name, code string }{
		{"eval", `eval("(".repeat(60000) + "1" + ")".repeat(60000))`},
		{"Function", `Function("return " + "(".repeat(60000) + "1" + ")".repeat(60000))`},
		{"constructor", `(function(){}).constructor("return " + "(".repeat(60000) + "1" + ")".repeat(60000))`},
		{"JSON.parse", `JSON.parse("[".repeat(300000) + "]".repeat(300000))`},
		{"RegExp", `new RegExp("(?=a)" + "(".repeat(60000) + "a" + ")".repeat(60000))`},
		{"match", `"a".match("(?=a)" + "(".repeat(60000) + "a" + ")".repeat(60000))`},
		{"split species", `RegExp.prototype[Symbol.split].call({ source: "(?=a)" + "(".repeat(60000) + "a" + ")".repeat(60000), flags: "", [Symbol.match]: true }, "x")`},
		{"native recursion", `var d = 0; function f() { d++; [1].forEach(f); } f()`},
		{"getter recursion", `var d = 0; function f() { d++; JSON.stringify({ get x() { return f(); } }); return 1; } f()`},
	}
	if os.Getenv("MOSES_HARDENED_CHILD") == "1" {
		debug.SetMaxStack(8 << 20)
		for _, a := range attacks {
			fmt.Printf("start %s\n", a.name)
			program, err := compileScript(a.name, a.code)
			if err == nil {
				err = runScript(program, map[string]interface{}{}, 20*time.Second, nil)
			}
			fmt.Printf("survived %s (%v)\n", a.name, err != nil)
		}
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHardenedRunSurvivesDeepInputs$")
	cmd.Env = append(os.Environ(), "MOSES_HARDENED_CHILD=1", "GOMEMLIMIT=512MiB")
	out, err := cmd.CombinedOutput()
	text := string(out)
	for _, a := range attacks {
		if !strings.Contains(text, "survived "+a.name+" (true)") {
			t.Errorf("%s: did not end in a script error", a.name)
		}
	}
	if err != nil || t.Failed() {
		t.Fatalf("child: %v\n%s", err, lastLines(text, 25))
	}
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
