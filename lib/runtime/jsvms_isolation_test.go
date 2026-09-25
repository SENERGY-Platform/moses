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
	"math"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/dop251/goja"
	"go.mongodb.org/mongo-driver/bson"
)

// sharedStateSurface is the api of one channel whose world state is the real
// state api over a map every channel of the test shares.
func sharedStateSurface(env *environment, shared map[string]interface{}, c *collector) map[string]interface{} {
	surface := c.surface()
	surface["world"] = map[string]interface{}{"state": jsStateApi(env, func() map[string]interface{} { return shared })}
	return surface
}

// TestAFunctionCannotBeSharedThroughState is the cross-channel path: a function
// stored by one channel and called by another would run inside the first
// channel's kept vm, outside the caller's timeout.
func TestAFunctionCannotBeSharedThroughState(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	env := &environment{id: "e"}
	shared := map[string]interface{}{}

	owner, err := compileScript("a", `var seen = typeof globalThis.injected;
		try { moses.world.state.set('f', function () { globalThis.injected = 'from B'; }); } catch (e) {}
		moses.service.send(seen);`)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := compileScript("b", `var f = moses.world.state.get('f');
		if (typeof f === 'function') { f(); }
		moses.service.send(typeof f);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	if err := runScriptIn(vms, gen, owner, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("owner run: %v", err)
	}
	if _, stored := shared["f"]; stored {
		t.Fatalf("a function was stored: %T", shared["f"])
	}
	c = &collector{}
	if err := runScriptIn(vms, gen, caller, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("caller run: %v", err)
	}
	if c.last == "function" {
		t.Fatal("the other channel received a callable")
	}
	c = &collector{}
	if err := runScriptIn(vms, gen, owner, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("owner rerun: %v", err)
	}
	if c.last != "undefined" {
		t.Fatalf("the other channel changed the owner's vm: %v", c.last)
	}
}

func TestStateAcceptsPlainDataAndRefusesTheRest(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	env := &environment{id: "e"}
	shared := map[string]interface{}{}
	ok, err := compileScript("ok", `moses.world.state.set('d', {a: [1, 'x', true, null], b: 2.5}); moses.service.send(1);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	if err := runScriptIn(vms, gen, ok, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("plain data was refused: %v", err)
	}
	for _, value := range []string{`function () {}`, `moses`, `moses.service.send`, `{f: function () {}}`, `new Date(0)`, `1n`} {
		refused, err := compileScript("r", `moses.world.state.set('x', `+value+`);`)
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		if err := runScriptIn(vms, gen, refused, sharedStateSurface(env, shared, c), time.Second, nil); err == nil {
			t.Errorf("%s was stored", value)
		}
	}
	if _, stored := shared["x"]; stored {
		t.Fatalf("a refused value reached the state: %T", shared["x"])
	}
}

// TestALateTimeoutCallbackCannotReachTheNextRun forces the interleaving of the
// timer race: the callback of run one is held until the run is over, so without
// waiting for it its interrupt would land in run two of the same kept vm.
func TestALateTimeoutCallbackCannotReachTheNextRun(t *testing.T) {
	timeoutCallbackDelay.Store(int64(100 * time.Millisecond))
	defer timeoutCallbackDelay.Store(0)
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `var until = Date.now() + moses.busy; while (Date.now() < until) {} moses.service.send(moses.busy);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	quick := c.surface()
	quick["busy"] = 20
	//the timer has fired well before this run ends, and its callback is held back
	//past the end, so the run finishes normally with the interrupt still pending
	if err := runScriptIn(vms, gen, prg, quick, time.Millisecond, nil); err != nil {
		t.Fatalf("run one: %v", err)
	}
	c = &collector{}
	slow := c.surface()
	slow["busy"] = 300
	if err := runScriptIn(vms, gen, prg, slow, 5*time.Second, nil); err != nil {
		t.Fatalf("the late interrupt of run one reached run two: %v", err)
	}
}

func TestASymbolGlobalDoesNotSurviveTheRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `var k = Symbol.for('count');
		globalThis[k] = (globalThis[k] || 0) + 1;
		globalThis[Symbol('hidden')] = 1;
		moses.service.send(globalThis[k]);`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c := &collector{}
		if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if v, _ := c.last.(int64); v != 1 {
			t.Fatalf("run %d sent %v, want 1 - a symbol global survived", i, c.last)
		}
	}
	if vms.live(prg) == nil {
		t.Fatal("the vm was not kept")
	}
}

// TestTheWrapperKeepsTheOldBindings pins the forms that differ from running the
// body at the top level: the api globals and arguments behave as they did, and
// this.<top-level var> is the documented difference.
func TestTheWrapperKeepsTheOldBindings(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	cases := []struct {
		code string
		want interface{}
	}{
		{`var moses = moses; moses.service.send(1);`, int64(1)},
		{`var console = console || {}; console.warn('x'); moses.service.send(typeof console.warn);`, "function"},
		{`moses.service.send(typeof arguments);`, "undefined"},
		{`'use strict'; var x = 2; moses.service.send(x * 2);`, int64(4)},
		{`var httpGet = httpGet; moses.service.send(typeof httpGet);`, "function"},
	}
	for _, c := range cases {
		if v, err := runOn(t, vms, gen, c.code, time.Second); err != nil || v != c.want {
			t.Errorf("%s: sent %v (%v), want %v", c.code, v, err, c.want)
		}
	}
	// documented: a top-level var is no property of the global object any more
	v, err := runOn(t, vms, gen, `var factor = 2; function calc() { return this.factor * 3; } moses.service.send(calc());`, time.Second)
	if f, _ := asFloat(v); err != nil || !math.IsNaN(f) {
		t.Errorf("this.<top-level var> changed from the documented NaN: %v %v", v, err)
	}
}

// TestABodyThatBreaksOutOfTheWrapperIsRefused: the body must parse on its own,
// which a body closing the wrapper to reach the top level does not.
func TestABodyThatBreaksOutOfTheWrapperIsRefused(t *testing.T) {
	for _, code := range []string{
		`}); let x = 1; (function () { moses.service.send(x);`,
		`}).call(this); var leak = 1; (function () {`,
		`moses.service.send(1); return;`,
	} {
		if _, err := compileScript("t", code); err == nil {
			t.Errorf("compiled: %s", code)
		}
	}
}

// TestTheKeptVMsAreCapped lowers both caps and checks that the least recently
// used vms go, per environment and across environments, and that an evicted
// channel simply gets a fresh vm.
func TestTheKeptVMsAreCapped(t *testing.T) {
	environmentCap, processCap := maxEnvironmentVMs, maxProcessVMs
	defer func() { maxEnvironmentVMs, maxProcessVMs = environmentCap, processCap }()
	maxEnvironmentVMs, maxProcessVMs = 3, 5

	gen := &generation{}
	one, two := newTestVMs(t), newTestVMs(t)
	run := func(vms *scriptVMs, n int) *goja.Program {
		prg, err := compileScript("c"+strconv.Itoa(n), `moses.service.send(`+strconv.Itoa(n)+`);`)
		if err != nil {
			t.Fatal(err)
		}
		c := &collector{}
		if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
			t.Fatalf("channel %d: %v", n, err)
		}
		if v, _ := c.last.(int64); v != int64(n) {
			t.Fatalf("channel %d sent %v", n, c.last)
		}
		return prg
	}

	first := run(one, 1)
	for n := 2; n <= 4; n++ {
		run(one, n)
	}
	if one.live(first) != nil {
		t.Fatal("the environment cap did not evict its least recently used vm")
	}
	if got := liveCount(one); got != 3 {
		t.Fatalf("expected 3 kept vms in the environment, have %d", got)
	}
	for n := 5; n <= 7; n++ {
		run(two, n)
	}
	if total := liveCount(one) + liveCount(two); total > 5 {
		t.Fatalf("the process cap was not held: %d kept vms", total)
	}
	// an evicted channel runs again on a fresh vm
	run(one, 1)
}

func liveCount(vms *scriptVMs) int {
	count := 0
	for _, slot := range vms.vms {
		if slot.current.Load() != nil {
			count++
		}
	}
	return count
}

// TestEvictionAcrossEnvironmentsIsRaceFree lowers the process cap so that
// environments running at the same time keep evicting each other's vms, which
// is the path that runs without the victim's lock; -race checks the handoff.
func TestEvictionAcrossEnvironmentsIsRaceFree(t *testing.T) {
	processCap := maxProcessVMs
	defer func() { maxProcessVMs = processCap }()
	maxProcessVMs = 2

	var wg sync.WaitGroup
	for e := 0; e < 4; e++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := &environment{id: "x"}
			defer func() {
				env.mux.Lock()
				env.scripts.clear()
				env.mux.Unlock()
			}()
			gen := &generation{}
			var programs []*goja.Program
			for n := 0; n < 3; n++ {
				prg, err := compileScript("t", `moses.service.send(`+strconv.Itoa(n)+`);`)
				if err != nil {
					t.Error(err)
					return
				}
				programs = append(programs, prg)
			}
			for i := 0; i < 30; i++ {
				n := i % len(programs)
				c := &collector{}
				if err := runScriptIn(&env.scripts, gen, programs[n], c.surface(), time.Second, &env.mux); err != nil {
					t.Error(err)
					return
				}
				if v, _ := c.last.(int64); v != int64(n) {
					t.Errorf("channel %d sent %v", n, c.last)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestAHiddenGlobalDoesNotSurviveTheRun: a non-enumerable global is cleaned too,
// since cleanup walks every own key of the global, not only enumerable ones.
func TestAHiddenGlobalDoesNotSurviveTheRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `var seen = typeof globalThis.hidden;
		Object.defineProperty(globalThis, 'hidden', {value: 1, enumerable: false, configurable: true});
		moses.service.send(seen);`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c := &collector{}
		if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if c.last != "undefined" {
			t.Fatalf("run %d saw a hidden global of an earlier run: %v", i, c.last)
		}
	}
	if vms.live(prg) == nil {
		t.Fatal("the vm was not kept")
	}
}

// TestABuiltinGlobalCannotBeReplacedForTheNextRun: an assignment to a builtin's
// name cannot carry a replacement into a later run of the kept vm.
func TestABuiltinGlobalCannotBeReplacedForTheNextRun(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `if (moses.replace) {
			try { Math = {max: function () { return 99; }}; } catch (e) {}
			try { globalThis.JSON = null; } catch (e) {}
		}
		moses.service.send(Math.max(1, 2) + (JSON === null ? 1000 : 0));`)
	if err != nil {
		t.Fatal(err)
	}
	for i, replace := range []bool{true, false} {
		c := &collector{}
		surface := c.surface()
		surface["replace"] = replace
		if err := runScriptIn(vms, gen, prg, surface, time.Second, nil); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if i == 1 {
			if v, _ := c.last.(int64); v != 2 {
				t.Fatalf("a replaced builtin reached the next run: %v", c.last)
			}
		}
	}
	if vms.live(prg) == nil {
		t.Fatal("the vm was not kept")
	}
}

// TestStateGetReturnsACopyNotTheLiveMap is the critical path: a script mutating
// what get returns must not write into the shared state, so it cannot smuggle a
// function into another channel's kept vm.
func TestStateGetReturnsACopyNotTheLiveMap(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	env := &environment{id: "e"}
	shared := map[string]interface{}{}

	owner, err := compileScript("a", `moses.world.state.set('o', {a: 1});
		var o = moses.world.state.get('o');
		o.f = function () { globalThis.injected = 'x'; };
		o.a = 999;
		moses.service.send(1);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	if err := runScriptIn(vms, gen, owner, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("owner run: %v", err)
	}
	stored := shared["o"].(map[string]interface{})
	if _, hasFunc := stored["f"]; hasFunc {
		t.Fatalf("a function was written into shared state through get: %T", stored["f"])
	}
	if a, _ := asFloat(stored["a"]); a != 1 {
		t.Fatalf("get handed out the live map: a became %v", stored["a"])
	}
	caller, err := compileScript("b", `var o = moses.world.state.get('o');
		moses.service.send(typeof o.f);`)
	if err != nil {
		t.Fatal(err)
	}
	c = &collector{}
	if err := runScriptIn(vms, gen, caller, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("caller run: %v", err)
	}
	if c.last != "undefined" {
		t.Fatalf("the other channel saw a callable in shared state: %v", c.last)
	}
}

// TestACyclicStoredStateDoesNotCrashTheSnapshot pins the depth-and-cycle guard:
// a cycle injected straight into the Go state map, which a script can no longer
// create, must be dropped rather than overflow the stack.
func TestACyclicStoredStateDoesNotCrashTheSnapshot(t *testing.T) {
	cyclic := map[string]interface{}{"n": 1.0}
	cyclic["self"] = cyclic
	deep := interface{}(1.0)
	for i := 0; i < maxStateCopyDepth+50; i++ {
		deep = []interface{}{deep}
	}
	state := repo.RuntimeState{
		EnvironmentId: "e",
		Context:       map[string]interface{}{"c": cyclic, "d": deep},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}
	// must return rather than recurse forever
	snapshot := snapshotState("e", state)
	// the cycle edge is dropped and the rest of the value kept
	c, ok := snapshot.Context["c"].(map[string]interface{})
	if !ok {
		t.Fatalf("the cyclic value lost its shape: %#v", snapshot.Context["c"])
	}
	if c["n"] != 1.0 {
		t.Fatalf("the plain part of the cyclic value was lost: %#v", c)
	}
	if c["self"] != nil {
		t.Fatalf("the cycle was copied instead of cut: %T", c["self"])
	}
	// the deep value is cut at the depth limit, not copied to its full depth
	levels := 0
	for v := snapshot.Context["d"]; ; levels++ {
		list, ok := v.([]interface{})
		if !ok || len(list) == 0 {
			break
		}
		v = list[0]
	}
	if levels != maxStateCopyDepth {
		t.Fatalf("the deep value kept %d levels, want the limit %d", levels, maxStateCopyDepth)
	}
}

// TestLexicalDeclarationsOfApiNamesCompileAndShadow pins finding 3: a top-level
// let/const/class binding an api name compiles (the name is not a parameter) and
// shadows the api, while var reading the api still works.
func TestLexicalDeclarationsOfApiNamesCompileAndShadow(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	// these must compile, unlike when the api name is a wrapper parameter
	for _, code := range []string{
		`const console = {warn: function () {}}; globalThis.__send(typeof console.warn);`,
		`let httpGet = 5; globalThis.__send(httpGet);`,
		`class moses {}; globalThis.__send(typeof moses);`,
		`const {console} = {console: 1};`,
		`let [httpGet] = [1];`,
		`const {a: {b: moses}} = {a: {b: 1}};`,
		`let [x, [, console = 2], ...httpGet] = [];`,
		`const {z = 1, ...moses} = {};`,
	} {
		if _, err := compileScript("t", code); err != nil {
			t.Errorf("did not compile: %s: %v", code, err)
		}
	}
	// var reading the api global still works, as it did on master
	if v, err := runOn(t, vms, gen, `var console = console || {}; console.warn('x'); moses.service.send(typeof console.warn);`, time.Second); err != nil || v != "function" {
		t.Errorf("var console = console || {} broke: %v %v", v, err)
	}
	// a destructured shadow is what the body reads, as on master
	if v, err := runOn(t, vms, gen, `const {console} = {console: 7}; moses.service.send(console);`, time.Second); err != nil || v != int64(7) {
		t.Errorf("a destructured console did not shadow: %v %v", v, err)
	}
	// a lexical shadow genuinely shadows the api: the class has no service, so the
	// send throws at run time rather than compiling to the api
	if _, err := runOn(t, vms, gen, `class moses {} moses.service.send(1);`, time.Second); err == nil {
		t.Error("a shadowing class still reached moses.service")
	}
}

// TestFreezingAPerRunGlobalDiscardsTheVM pins finding 4: a run that makes moses
// non-writable must not leave a vm on which the next run's set of moses fails.
func TestFreezingAPerRunGlobalDiscardsTheVM(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `if (moses.freeze) { Object.defineProperty(globalThis, 'moses', {writable: false}); }
		moses.service.send(1);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	surface := c.surface()
	surface["freeze"] = true
	if err := runScriptIn(vms, gen, prg, surface, time.Second, nil); err != nil {
		t.Fatalf("the freezing run itself failed: %v", err)
	}
	if vms.live(prg) != nil {
		t.Fatal("the vm was kept although a per-run global was frozen")
	}
	// the next run gets a fresh vm and works
	c = &collector{}
	surface = c.surface()
	surface["freeze"] = false
	if err := runScriptIn(vms, gen, prg, surface, time.Second, nil); err != nil {
		t.Fatalf("the next run failed on a broken vm: %v", err)
	}
	if v, _ := c.last.(int64); v != 1 {
		t.Fatalf("the next run sent %v", c.last)
	}
}

// TestAZeroEnvironmentCapKeepsNothingOnTheProcessList pins the minor fix: with
// the environment cap at zero, a program is evicted at once and never joins the
// process order.
func TestAZeroEnvironmentCapKeepsNothingOnTheProcessList(t *testing.T) {
	environmentCap := maxEnvironmentVMs
	defer func() { maxEnvironmentVMs = environmentCap }()
	maxEnvironmentVMs = 0

	processVMs.mux.Lock()
	before := processVMs.order.Len()
	processVMs.mux.Unlock()

	vms := newTestVMs(t)
	gen := &generation{}
	prg, err := compileScript("t", `moses.service.send(1);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	if err := runScriptIn(vms, gen, prg, c.surface(), time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if vms.live(prg) != nil {
		t.Fatal("a program was kept although the environment cap is zero")
	}
	processVMs.mux.Lock()
	after := processVMs.order.Len()
	processVMs.mux.Unlock()
	if after != before {
		t.Fatalf("a dropped slot joined the process order: %d -> %d", before, after)
	}
}

// TestSetStoresACopyTheScriptCannotReach pins that set stores a fresh copy: a
// value obtained from get, stored, then mutated - a function, a cycle, deep
// nesting - leaves the stored state unchanged, clean and encodable.
func TestSetStoresACopyTheScriptCannotReach(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	env := &environment{id: "e"}
	shared := map[string]interface{}{}
	prg, err := compileScript("a", `var s = moses.world.state;
		s.set('o', {v: 1});
		var o = s.get('o');
		s.set('p', o);
		o.f = function () {};
		o.self = o;
		var d = o;
		for (var i = 0; i < 100; i++) { d.x = {}; d = d.x; }
		var a = [1];
		s.set('q', a);
		a.push(function () {});
		a.push(a);
		moses.service.send(1);`)
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	if err := runScriptIn(vms, gen, prg, sharedStateSurface(env, shared, c), time.Second, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	p := shared["p"].(map[string]interface{})
	//lengths only: a stored value the script reached may be cyclic now
	if len(p) != 1 {
		t.Fatalf("the stored copy changed after set: it has %d keys", len(p))
	}
	if q := shared["q"].([]interface{}); len(q) != 1 {
		t.Fatalf("the stored array changed after set: it has %d elements", len(q))
	}
	if _, err := json.Marshal(snapshotState("e", repo.RuntimeState{Context: shared})); err != nil {
		t.Fatalf("the stored state no longer encodes as json: %v", err)
	}
	if _, err := bson.Marshal(map[string]interface{}{"context": shared}); err != nil {
		t.Fatalf("the stored state no longer encodes as bson: %v", err)
	}
}

// dag builds a value that shares its subtrees: depth levels, each pointing twice
// at the one below, so it has depth+1 nodes but 2^depth paths.
func dag(depth int) map[string]interface{} {
	current := map[string]interface{}{"leaf": 1.0}
	for i := 0; i < depth; i++ {
		current = map[string]interface{}{"l": current, "r": current}
	}
	return current
}

// TestASharedSubtreeValueIsRefusedQuickly pins the node budget: a value whose
// copy would walk 2^28 paths is refused by set, and dropped by get and by the
// snapshot, within milliseconds rather than exhausting time or memory.
func TestASharedSubtreeValueIsRefusedQuickly(t *testing.T) {
	vms := newTestVMs(t)
	gen := &generation{}
	env := &environment{id: "e"}
	shared := map[string]interface{}{}
	prg, err := compileScript("a", `var cur = {leaf: 1};
		for (var i = 0; i < 28; i++) { var n = {}; n.l = cur; n.r = cur; cur = n; }
		moses.world.state.set('dag', cur);`)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	c := &collector{}
	if err := runScriptIn(vms, gen, prg, sharedStateSurface(env, shared, c), 5*time.Second, nil); err == nil {
		t.Fatal("set accepted a value that shares its subtrees 28 levels deep")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("refusing the value took %v", elapsed)
	}
	if _, stored := shared["dag"]; stored {
		t.Fatal("the refused value reached the state")
	}

	// the same shape injected straight into a state, as only corruption could
	started = time.Now()
	shared["dag"] = dag(28)
	getter, err := compileScript("b", `moses.service.send(moses.world.state.get('dag') === null ? 'dropped' : 'read');`)
	if err != nil {
		t.Fatal(err)
	}
	c = &collector{}
	if err := runScriptIn(vms, gen, getter, sharedStateSurface(env, shared, c), 5*time.Second, nil); err != nil {
		t.Fatalf("get: %v", err)
	}
	if c.last != "dropped" {
		t.Fatalf("get read a value past the node budget: %v", c.last)
	}
	snapshot := snapshotState("e", repo.RuntimeState{Context: shared})
	if _, kept := snapshot.Context["dag"]; kept {
		t.Fatal("the snapshot kept a value past the node budget")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("dropping the value took %v", elapsed)
	}
}
