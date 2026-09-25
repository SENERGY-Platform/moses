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
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/robertkrimen/otto"
)

func hardenedOtto(t *testing.T) *otto.Otto {
	t.Helper()
	vm := otto.New()
	if err := hardenVM(vm); err != nil {
		t.Fatalf("hardening failed: %v", err)
	}
	return vm
}

func ottoTrue(t *testing.T, vm *otto.Otto, code string) {
	t.Helper()
	v, err := vm.Run(code)
	if err != nil {
		t.Errorf("%s: %v", code, err)
		return
	}
	if b, _ := v.ToBoolean(); !b {
		t.Errorf("%s: false", code)
	}
}

func ottoThrows(t *testing.T, vm *otto.Otto, code, fragment string) {
	t.Helper()
	_, err := vm.Run(code)
	if err == nil {
		t.Errorf("%s: did not throw", code)
	} else if !strings.Contains(err.Error(), fragment) {
		t.Errorf("%s: %v lacks %q", code, err, fragment)
	}
}

func TestLegacyCodeFromStringsIsUnavailable(t *testing.T) {
	vm := hardenedOtto(t)
	for _, code := range []string{
		`eval("1")`,
		`(function () { var local = 1; return eval("local"); })()`,
		`this.eval("1")`,
		`Function("return 1")()`,
		`new Function("return 1")`,
		`(function () {}).constructor("return 1")`,
		`Function.prototype.constructor("return 1")`,
	} {
		ottoThrows(t, vm, code, "not available in MOSES scripts")
	}
	ottoTrue(t, vm, `(function () {}) instanceof Function`)
}

func TestLegacyRegExpIsGuardedAndStillWorks(t *testing.T) {
	vm := hardenedOtto(t)
	ottoTrue(t, vm, `new RegExp("a+", "g").test("caab") && RegExp("b").test("b") && /x/ instanceof RegExp`)
	ottoTrue(t, vm, `/x/.constructor === RegExp && "a1b2".match("\\d")[0] === "1" && "abc".search("c") === 2`)
	ottoTrue(t, vm, `"a,b".split(/,/).length === 2 && "xaax".replace(/a/g, "b") === "xbbx" && "a1".match(/\d/)[0] === "1"`)
	deep := `(function () { var s = ""; for (var i = 0; i < 201; i++) s += "("; s += "a"; for (i = 0; i < 201; i++) s += ")"; return s; })()`
	for _, code := range []string{`new RegExp(` + deep + `)`, `RegExp(` + deep + `)`, `"a".match(` + deep + `)`, `"a".search(` + deep + `)`} {
		ottoThrows(t, vm, code, "regular expression nests")
	}
	ottoTrue(t, vm, `var n = 0; var sly = { toString: function () { n++; return n === 1 ? "ab" : "((((("; } };
		new RegExp(sly).source === "ab" && n === 1`)
}

func TestLegacyRunRefusesAComplexScript(t *testing.T) {
	err := run(strings.Repeat("(", 5000)+"1"+strings.Repeat(")", 5000), map[string]interface{}{}, time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "deep") {
		t.Fatalf("expected the complexity error, got %v", err)
	}
}

// TestLegacyRunSurvivesDeepInputs runs every known crash input through run() in
// a child under an 8 MB max stack, where each overflows unguarded.
func TestLegacyRunSurvivesDeepInputs(t *testing.T) {
	// doubling, since appending one character at a time is quadratic in otto
	build := func(open, inner, close string, n int) string {
		return fmt.Sprintf(`(function () { function rep(c, n) { var s = c; while (s.length < n) s += s; return s.substring(0, n); } return rep(%q, %d) + %q + rep(%q, %d); })()`, open, n, inner, close, n)
	}
	attacks := []struct{ name, code string }{
		{"stored deep script", strings.Repeat("(", 30000) + "1" + strings.Repeat(")", 30000)},
		{"eval", `eval(` + build("(", "1", ")", 60000) + `)`},
		{"Function", `Function("return " + ` + build("(", "1", ")", 60000) + `)`},
		{"constructor", `(function () {}).constructor("return " + ` + build("(", "1", ")", 60000) + `)`},
		{"RegExp", `new RegExp(` + build("(", "a", ")", 200000) + `)`},
		{"match", `"a".match(` + build("(", "a", ")", 200000) + `)`},
		{"recursion", `function f() { f(); } f()`},
		{"join", `var a = []; for (var i = 0; i < 100000; i++) a = [a]; String(a)`},
	}
	if os.Getenv("MOSES_LEGACY_CHILD") == "1" {
		debug.SetMaxStack(8 << 20)
		for _, a := range attacks {
			fmt.Printf("start %s\n", a.name)
			err := run(a.code, map[string]interface{}{}, 20*time.Second, nil)
			fmt.Printf("survived %s (%v)\n", a.name, err != nil)
		}
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLegacyRunSurvivesDeepInputs$")
	cmd.Env = append(os.Environ(), "MOSES_LEGACY_CHILD=1", "GOMEMLIMIT=512MiB")
	out, err := cmd.CombinedOutput()
	text := string(out)
	for _, a := range attacks {
		if !strings.Contains(text, "survived "+a.name+" (true)") {
			t.Errorf("%s: did not end in a script error", a.name)
		}
	}
	if err != nil || t.Failed() {
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
		if len(lines) > 25 {
			lines = lines[len(lines)-25:]
		}
		t.Fatalf("child: %v\n%s", err, strings.Join(lines, "\n"))
	}
}
