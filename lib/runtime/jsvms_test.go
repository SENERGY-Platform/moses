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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// collector is a moses surface whose send remembers the last value, so a test
// can read what a run produced.
type collector struct {
	last interface{}
}

func (c *collector) surface() map[string]interface{} {
	return map[string]interface{}{
		"service": map[string]interface{}{"send": func(v interface{}) { c.last = v }},
	}
}

// runOn runs one script on a shared cache, as the runtime does, and returns the
// value it sent.
func runOn(t *testing.T, vms *scriptVMs, gen *generation, code string, timeout time.Duration) (interface{}, error) {
	t.Helper()
	prg, err := compileScript("t", code)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	c := &collector{}
	err = runScriptIn(vms, gen, prg, c.surface(), timeout, nil)
	return c.last, err
}

func TestAnImplicitGlobalDoesNotSurviveTheRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `if (typeof leak === "undefined") { leak = 0; } leak = leak + 1; moses.service.send(leak);`)
	if err != nil {
		t.Fatal(err)
	}
	// the same program on the same vm twice: an implicit global set in run one
	// must be gone in run two, or the count would climb
	for i := 0; i < 3; i++ {
		c := &collector{}
		if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if v, _ := c.last.(int64); v != 1 {
			t.Fatalf("run %d sent %v, want 1 - a global survived the run", i, c.last)
		}
	}
	// and the vm was reused, not discarded
	if len(vms.vms) != 1 {
		t.Fatalf("expected the vm to be kept, have %d", len(vms.vms))
	}
}

func TestDeclaredVarsAndFunctionsAreFreshEachRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	code := `var n = (typeof n === "undefined") ? 0 : n; function f() { return 41; } n = n + 1; moses.service.send(f() + n);`
	prg, _ := compileScript("t", code)
	for i := 0; i < 2; i++ {
		c := &collector{}
		if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
			t.Fatal(err)
		}
		if v, _ := c.last.(int64); v != 42 {
			t.Fatalf("run %d sent %v, want 42", i, c.last)
		}
	}
}

func TestABuiltinMutationDoesNotPersist(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	// one program on its kept vm: the first run tries to poison Array.prototype
	// and Object.prototype, the second must see neither
	prg, err := compileScript("t", `if (moses.poison) {
			try { Array.prototype.push = function () { return 99; }; } catch (e) {}
			try { Object.prototype.tainted = 1; } catch (e) {}
		}
		var a = []; a.push(7); moses.service.send(a.length + ("tainted" in {} ? 100 : 0));`)
	if err != nil {
		t.Fatal(err)
	}
	for i, poison := range []bool{true, false} {
		c := &collector{}
		surface := c.surface()
		surface["poison"] = poison
		if err := runScriptIn(vms, gen, prg, surface, time.Second, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if v, _ := c.last.(int64); v != 1 {
			t.Fatalf("run %d sent %v, want 1 - a builtin was changed", i, c.last)
		}
	}
	if vms.live(prg) == nil {
		t.Fatal("the vm was not kept, so the second run did not test anything")
	}
}

func TestAnOwnPropertyOverridingABuiltinNameStillWorks(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	// assigning toString on an own object, although Object.prototype is frozen
	code := `var o = {}; o.toString = function () { return "custom"; }; moses.service.send(String(o) + Object.prototype.hasOwnProperty.call(o, "toString"));`
	v, err := runOn(t, vms, gen, code, time.Second)
	if err != nil || v.(string) != "customtrue" {
		t.Fatalf("own override refused: %v %v", v, err)
	}
}

func TestTwoChannelsDoNotShareState(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	// two different programs get two different vms; a leak in one is invisible to
	// the other even though they share the cache
	a, _ := compileScript("a", `globalThis.shared = 5; moses.service.send(1);`)
	b, _ := compileScript("b", `moses.service.send(typeof globalThis.shared === "undefined" ? "isolated" : "leaked");`)
	ca := &collector{}
	if err := runScriptIn(vms, gen, a, ca.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	cb := &collector{}
	if err := runScriptIn(vms, gen, b, cb.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if cb.last != "isolated" {
		t.Fatalf("channels shared a global: %v", cb.last)
	}
}

func TestATimeoutDiscardsTheVM(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	loop, _ := compileScript("t", `while (true) {}`)
	c := &collector{}
	if err := runScriptIn(vms, gen, loop, c.surface(), 150*time.Millisecond, nil); err != ErrScriptTimeout {
		t.Fatalf("expected the timeout, got %v", err)
	}
	if len(vms.vms) != 0 {
		t.Fatal("an interrupted run left its vm in the cache")
	}
	// the next run of the same program gets a fresh, working vm
	if v, err := runOn(t, vms, gen, `moses.service.send(1 + 1);`, time.Second); err != nil || v.(int64) != 2 {
		t.Fatalf("the vm did not recover after a timeout: %v %v", v, err)
	}
}

func TestAStackOverflowDiscardsTheVM(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	deep, _ := compileScript("t", `var d = 0; function f() { d++; [1].forEach(f); } f();`)
	c := &collector{}
	if err := runScriptIn(vms, gen, deep, c.surface(), time.Second, nil); err == nil {
		t.Fatal("the deep recursion should have failed")
	}
	if len(vms.vms) != 0 {
		t.Fatal("a stack overflow left its vm in the cache")
	}
	if v, err := runOn(t, vms, gen, `moses.service.send(3);`, time.Second); err != nil || v.(int64) != 3 {
		t.Fatalf("the vm did not recover after an overflow: %v %v", v, err)
	}
}

func TestANewGenerationDiscardsTheVMs(t *testing.T) {
	vms := newTestVMs(t)
	genA := &generation{}
	prg, _ := compileScript("t", `moses.service.send(1);`)
	c := &collector{}
	if err := runScriptIn(vms, genA, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	firstVM := vms.live(prg)
	if firstVM == nil {
		t.Fatal("no vm was cached")
	}
	genB := &generation{}
	if err := runScriptIn(vms, genB, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if vms.gen != genB {
		t.Fatal("the cache did not adopt the new generation")
	}
	if vms.live(prg) == firstVM {
		t.Fatal("the new generation reused the old vm")
	}
}

func TestTopLevelReturnIsRefusedAndTopLevelVarIsNotAGlobal(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	// the body has to parse on its own, so a top-level return stays a syntax
	// error exactly as it was before the wrapper
	if _, err := compileScript("t", `moses.service.send(7); return;`); err == nil {
		t.Fatal("a top-level return compiled")
	}
	// a top-level var is a function local now, not a property of the global
	if v, err := runOn(t, vms, gen, `var top = 1; moses.service.send(typeof globalThis.top);`, time.Second); err != nil || v.(string) != "undefined" {
		t.Fatalf("top-level var leaked to the global: %v %v", v, err)
	}
}

func TestManyAddedGlobalsForceAFreshVM(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	code := `for (var i = 0; i < ` + itoa(maxAddedGlobals+5) + `; i++) { globalThis["g" + i] = i; } moses.service.send(1);`
	if _, err := runOn(t, vms, gen, code, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if len(vms.vms) != 0 {
		t.Fatal("a run past the cleanup bound kept its vm")
	}
}

// TestConcurrentEnvironmentsDoNotShareVMs runs many script channels at once. The
// guarantee is that a vm is only ever touched under its environment's mux; the
// race detector is what turns a violation into a failure here.
func TestConcurrentEnvironmentsDoNotShareVMs(t *testing.T) {
	const envs = 6
	var wg sync.WaitGroup
	for e := 0; e < envs; e++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := &environment{id: "x"}
			vms := &env.scripts
			defer func() {
				env.mux.Lock()
				env.scripts.clear()
				env.mux.Unlock()
			}()
			gen := &generation{}
			prg, err := compileScript("t", `if (typeof c === "undefined") c = 0; c++; moses.service.send(c);`)
			if err != nil {
				t.Error(err)
				return
			}
			for i := 0; i < 50; i++ {
				c := &collector{}
				if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, &env.mux); err != nil {
					t.Error(err)
					return
				}
				if v, _ := c.last.(int64); v != 1 {
					t.Errorf("state leaked between runs: %v", c.last)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestScriptEquivalenceWithThePreviousRunner runs each script the way the runtime
// did before the vm was kept - the raw code compiled on its own, a fresh
// unhardened vm per run, the same api - and on a kept hardened vm, for many
// ticks, and requires identical output. Set MOSES_DEMONSTRATOR to a musterwerke
// directory to run every one of its scripts as well.
func TestScriptEquivalenceWithThePreviousRunner(t *testing.T) {
	scripts := append(equivalenceScripts(), demonstratorScripts(t)...)
	for _, code := range scripts {
		previous, err := goja.Compile("t", code, false)
		if err != nil {
			t.Fatalf("a script did not compile on the previous runner: %v", err)
		}
		prg, err := compileScript("t", code)
		if err != nil {
			t.Fatalf("a script that compiled before is refused now: %v\n%s", err, head(code))
		}
		reused := newTestVMs(t)
		gen := &generation{}
		for tick := 0; tick < 200; tick++ {
			seed := float64(tick)
			before, now := newEquivState(seed), newEquivState(seed)

			beforeErr := runPreviously(previous, before.surface())
			nowErr := runScriptIn(reused, gen, prg, now.surface(), 2*time.Second, nil)

			if (beforeErr == nil) != (nowErr == nil) {
				t.Fatalf("tick %d: previously %v, now %v\n%s", tick, beforeErr, nowErr, head(code))
			}
			if !before.equal(now) {
				t.Fatalf("tick %d: previously %v, now %v\n%s", tick, before.sent, now.sent, head(code))
			}
		}
		if reused.live(prg) == nil {
			t.Fatalf("the vm was not kept, so the comparison did not cover reuse\n%s", head(code))
		}
	}
}

// runPreviously is the runner as it was: a fresh vm, nothing hardened, frozen or
// wrapped.
func runPreviously(program *goja.Program, moses interface{}) error {
	vm := goja.New()
	if err := vm.Set("moses", moses); err != nil {
		return err
	}
	if err := vm.Set("httpGet", httpGet); err != nil {
		return err
	}
	if err := vm.Set("console", scriptConsole()); err != nil {
		return err
	}
	_, err := vm.RunProgram(program)
	return err
}

func head(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 120 {
		code = code[:120]
	}
	return code
}

// equivState is a moses surface backed by a deterministic, seeded store, so two
// runs seeded alike answer every get identically and can be compared on what
// they sent and what they wrote.
type equivState struct {
	seed  float64
	store map[string]float64
	sent  []interface{}
}

func newEquivState(seed float64) *equivState {
	return &equivState{seed: seed, store: map[string]float64{}}
}

func (s *equivState) get(scope string, key interface{}) interface{} {
	k := scope + ":" + fmtKey(key)
	if v, ok := s.store[k]; ok {
		return v
	}
	// a deterministic pseudo value per key and seed, spanning zero and negatives
	h := 0.0
	for _, r := range k {
		h = h*31 + float64(r)
	}
	return float64(int64(h+s.seed*7)%97) / 13.0
}

func (s *equivState) set(scope string, key interface{}, value interface{}) {
	k := scope + ":" + fmtKey(key)
	if f, ok := asFloat(value); ok {
		s.store[k] = f
	} else {
		s.store[k] = 0
	}
}

func fmtKey(key interface{}) string {
	if k, ok := key.(string); ok {
		return k
	}
	return ""
}

func (s *equivState) stateApi(scope string) map[string]interface{} {
	return map[string]interface{}{
		"get": func(key interface{}) interface{} { return s.get(scope, key) },
		"set": func(key interface{}, value interface{}) { s.set(scope, key, value) },
	}
}

// surface has the shape of the runtime's api: scopes with their state, the
// lookups between them, the channel with its send, and the aliases.
func (s *equivState) surface() map[string]interface{} {
	asset := func(id string) map[string]interface{} {
		return map[string]interface{}{"state": s.stateApi("asset/" + id)}
	}
	zone := func(id string) map[string]interface{} {
		return map[string]interface{}{
			"state":     s.stateApi("zone/" + id),
			"getDevice": func(assetId string) map[string]interface{} { return asset(assetId) },
		}
	}
	environment := map[string]interface{}{
		"state":   s.stateApi("environment"),
		"getRoom": func(zoneId string) map[string]interface{} { return zone(zoneId) },
	}
	own, ownZone := asset("own"), zone("own")
	channel := map[string]interface{}{
		"input": nil,
		"send":  func(v interface{}) { s.sent = append(s.sent, normalizeSent(v)) },
	}
	return map[string]interface{}{
		"world": environment, "room": ownZone, "device": own, "service": channel,
		"environment": environment, "zone": ownZone, "asset": own, "channel": channel,
	}
}

// normalizeSent reduces a sent value to something comparable across runs.
func normalizeSent(v interface{}) interface{} {
	if f, ok := asFloat(v); ok {
		return f
	}
	return v
}

func (s *equivState) equal(other *equivState) bool {
	if len(s.sent) != len(other.sent) {
		return false
	}
	for i := range s.sent {
		if s.sent[i] != other.sent[i] {
			return false
		}
	}
	if len(s.store) != len(other.store) {
		return false
	}
	for k, v := range s.store {
		if other.store[k] != v {
			return false
		}
	}
	return true
}

// equivalenceScripts are checked-in scripts in the shape of the demonstrator's:
// they exercise the builtins scripts actually use (Math, Array, String, JSON,
// arithmetic, the state and send api), so the equivalence test runs everywhere.
func equivalenceScripts() []string {
	return []string{
		`moses.service.send(1);`,
		`var s = moses.asset.state; var v = (s.get('n') || 0) + 1; s.set('n', v); moses.service.send(v);`,
		`var e = moses.environment.state; var x = e.get('temp'); if (x !== x || x < -50) x = 15; moses.service.send(Math.max(0, Math.min(100, x * 1.5 + 2)));`,
		`var d = [312, 4, 10, 12, 168]; var t = 0; for (var i = 0; i < d.length; i++) t += d[i]; moses.service.send(t % 97);`,
		`var s = moses.asset.state; moses.service.send(Math.round(Math.sin(s.get('n') || 0) * 100) / 100);`,
		`var parts = "a,b,c".split(","); moses.service.send(parts.length + "x".length);`,
		`var o = { a: [1, { b: 2 }], f: function () { return 3; } }; moses.service.send(o.a[1].b + o.f());`,
		`var acc = 0; [1, 2, 3, 4].forEach(function (n) { acc += n * n; }); moses.service.send(acc);`,
		`var r = /^[a-z]+(\d+)$/.exec("ab12"); moses.service.send(r ? Number(r[1]) : -1);`,
		`var e = moses.environment.state; e.set('m', (e.get('m') || 0) + 2); moses.service.send(JSON.parse("[1,2,3]").length + e.get('m'));`,
		`function clamp(v, lo, hi) { return v < lo ? lo : v > hi ? hi : v; } moses.service.send(clamp(moses.asset.state.get('p') || 0, 0, 10));`,
		`var now = moses.asset.state.get('k') || 0; var s = ""; for (var i = 0; i < 3; i++) s += i; moses.service.send(now + s.length);`,
	}
}

// demonstratorScripts reads the scripts of both documents the demonstrator keeps,
// the source and the rendered one.
func demonstratorScripts(t *testing.T) []string {
	t.Helper()
	dir := os.Getenv("MOSES_DEMONSTRATOR")
	if dir == "" {
		return nil
	}
	var out []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case map[string]interface{}:
			for k, y := range x {
				if s, ok := y.(string); ok && k == "code" {
					out = append(out, s)
				} else {
					walk(y)
				}
			}
		case []interface{}:
			for _, y := range x {
				walk(y)
			}
		}
	}
	for _, name := range []string{"environment.json", "environment.rendered.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("MOSES_DEMONSTRATOR is set but %s is unreadable: %v", name, err)
		}
		var doc interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unable to parse %s: %v", name, err)
		}
		walk(doc)
	}
	return out
}

// TestALatchedInterruptDiscardsAKeptVM pins the reused-path counterpart of the
// timer race: a vm whose interrupt was latched after its run cannot be reused,
// because its cleanup call aborts; the next run of the program gets a fresh one
// and works.
func TestALatchedInterruptDiscardsAKeptVM(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, _ := compileScript("t", `moses.service.send(1);`)
	c := &collector{}
	if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	kept := vms.live(prg)
	if kept == nil {
		t.Fatal("the vm was not kept after a normal run")
	}
	// latch an interrupt on the idle kept vm, as a timer firing late would
	kept.vm.Interrupt("stale")
	if kept.restore() {
		t.Fatal("restore reported clean although the vm was interrupted")
	}
	// the next run recovers: ClearInterrupt at its start clears the stale flag
	if v, err := runOn(t, vms, gen, `moses.service.send(9);`, time.Second); err != nil || v.(int64) != 9 {
		t.Fatalf("a run after a latched interrupt failed: %v %v", v, err)
	}
}

// TestAnInterruptLandingAfterCleanupDoesNotFailTheNextRun covers the timer
// callback that runs after the run's cleanup already succeeded: the kept vm
// then carries the interrupt, and the next run must clear it, not time out.
func TestAnInterruptLandingAfterCleanupDoesNotFailTheNextRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, _ := compileScript("t", `moses.service.send(5);`)
	c := &collector{}
	if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	kept := vms.live(prg)
	if kept == nil {
		t.Fatal("the vm was not kept after a normal run")
	}
	kept.vm.Interrupt(ErrScriptTimeout)
	c = &collector{}
	if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatalf("a stale interrupt reached the next run: %v", err)
	}
	if v, _ := c.last.(int64); v != 5 {
		t.Fatalf("the next run sent %v, want 5", c.last)
	}
}

// newTestVMs is a cache that is cleared when the test ends, so its vms do not
// stay in the process-wide order for the rest of the test binary.
func newTestVMs(t *testing.T) *scriptVMs {
	t.Helper()
	vms := &scriptVMs{}
	t.Cleanup(vms.clear)
	return vms
}
