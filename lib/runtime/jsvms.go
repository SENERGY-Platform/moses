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
	"container/list"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
)

// A script channel keeps one prepared vm across its runs, because preparing one
// costs far more than a run. Isolation between runs is restored instead: every
// builtin is frozen and sits on a frozen prototype of the global object, and
// whatever a run adds to the global object is deleted after it.

// maxAddedGlobals bounds what cleanup deletes one by one; a run that added more
// gets a fresh vm next time, which is cheaper than walking them.
const maxAddedGlobals = 1000

// isolationSource freezes the vm and returns the cleanup a run ends with.
// Properties an ordinary script assigns on its own objects (toString, message,
// ...) become accessors first, so a frozen prototype does not turn such an
// assignment into a silent no-op.
const isolationSource = `(function (global, maxAdded) {
	'use strict';
	var freeze = Object.freeze, prototypeOf = Object.getPrototypeOf, ownKeys = Reflect.ownKeys;
	var describe = Object.getOwnPropertyDescriptor, defineProperty = Object.defineProperty;
	var isExtensible = Object.isExtensible, deleteProperty = Reflect.deleteProperty;
	var createObject = Object.create, setPrototypeOf = Object.setPrototypeOf;
	var TypeErrorType = TypeError, SetType = Set;

	var perRun = ['moses', 'httpGet', 'console'];
	for (var i = 0; i < perRun.length; i++) {
		defineProperty(global, perRun[i], {value: undefined, writable: true, enumerable: false, configurable: false});
	}

	var overridable = ['constructor', 'toString', 'valueOf', 'toLocaleString', 'toJSON', 'name', 'message',
		'hasOwnProperty', 'isPrototypeOf', 'propertyIsEnumerable'];
	function allowOverride(target, key) {
		var d = describe(target, key);
		if (!d || !('value' in d) || !d.writable || !d.configurable) return;
		var value = d.value;
		defineProperty(target, key, {
			get: function () { return value; },
			set: function (v) {
				if (this === target) throw new TypeErrorType('builtins are read-only in MOSES scripts');
				if (this === null || (typeof this !== 'object' && typeof this !== 'function')) return;
				defineProperty(this, key, {value: v, writable: true, enumerable: true, configurable: true});
			},
			enumerable: d.enumerable, configurable: false
		});
	}

	// intrinsics no global property leads to
	var roots = [
		prototypeOf(function* () {}), prototypeOf(async function () {}),
		prototypeOf([][Symbol.iterator]()), prototypeOf(''[Symbol.iterator]()),
		prototypeOf(new Map()[Symbol.iterator]()), prototypeOf(new Set()[Symbol.iterator]()),
		prototypeOf(/a/g[Symbol.matchAll]('')), prototypeOf(Int8Array)
	];
	var callee = describe((function () { 'use strict'; return arguments; })(), 'callee');
	if (callee) roots.push(callee.get, callee.set);

	// First pass, read-only: collect every builtin object reachable from the
	// global object and the rootless intrinsics. It must not create objects, or
	// the traversal would chase what it creates without end.
	var seen = new SetType([global]), pending = roots.slice(), all = [];
	var keys = ownKeys(global);
	for (i = 0; i < keys.length; i++) {
		var entry = describe(global, keys[i]);
		pending.push(entry.value, entry.get, entry.set);
	}
	pending.push(prototypeOf(global));
	while (pending.length > 0) {
		var o = pending.pop();
		if (o === null || (typeof o !== 'object' && typeof o !== 'function') || seen.has(o)) continue;
		seen.add(o);
		all.push(o);
		var own = ownKeys(o);
		for (var j = 0; j < own.length; j++) {
			var d = describe(o, own[j]);
			pending.push(d.value, d.get, d.set);
		}
		pending.push(prototypeOf(o));
	}

	// Second pass: make the overridable data properties settable on a receiver,
	// then freeze. The accessors created here are not traversed, so nothing grows.
	for (i = 0; i < all.length; i++) {
		var target = all[i];
		if (typeof target !== 'function') {
			for (var k = 0; k < overridable.length; k++) allowOverride(target, overridable[k]);
		}
		freeze(target);
	}

	// The builtin bindings move onto a frozen shelf that becomes the global's
	// prototype: names still resolve through it, but the global itself keeps only
	// the per-run bindings, so cleanup has little to enumerate.
	var shelf = createObject(prototypeOf(global));
	for (i = 0; i < keys.length; i++) {
		var g = describe(global, keys[i]);
		if (!g.configurable) continue;
		defineProperty(shelf, keys[i], g);
		deleteProperty(global, keys[i]);
	}
	freeze(shelf);
	setPrototypeOf(global, shelf);

	// Every key left on the global is non-configurable, so no run can delete one
	// and the key count only grows: an unchanged count means nothing was added.
	var known = new SetType(ownKeys(global)), globalPrototype = prototypeOf(global);
	return function cleanup() {
		var now = ownKeys(global), clean = true;
		if (now.length > known.size + maxAdded) return false;
		if (now.length !== known.size) {
			for (var k = 0; k < now.length; k++) {
				if (!known.has(now[k]) && !deleteProperty(global, now[k])) clean = false;
			}
		}
		// the per-run bindings have to be writable data properties still, or the
		// next run's set of moses would fail on a vm a script left half-broken
		for (var r = 0; r < perRun.length; r++) {
			var d = describe(global, perRun[r]);
			if (!d || !('value' in d) || !d.writable || d.enumerable || d.configurable) return false;
		}
		return clean && isExtensible(global) && prototypeOf(global) === globalPrototype;
	};
})`

var isolationProgram = mustCompilePrelude("moses-isolation", isolationSource)

func mustCompilePrelude(name string, source string) *goja.Program {
	prg, err := goja.Parse(name, source, parser.WithDisableSourceMaps)
	if err != nil {
		panic(err)
	}
	program, err := goja.CompileAST(prg, true)
	if err != nil {
		panic(err)
	}
	return program
}

// scriptVM is one prepared vm and the cleanup that restores it after a run.
type scriptVM struct {
	vm      *goja.Runtime
	cleanup goja.Callable
}

// newScriptVM hardens and freezes a fresh vm; it costs a few milliseconds, which
// is why a channel keeps its vm.
func newScriptVM() (*scriptVM, error) {
	vm := goja.New()
	if err := hardenVM(vm); err != nil {
		return nil, err
	}
	installer, err := vm.RunProgram(isolationProgram)
	if err != nil {
		return nil, err
	}
	install, ok := goja.AssertFunction(installer)
	if !ok {
		return nil, errors.New("the script isolation did not evaluate to a function")
	}
	cleanupValue, err := install(goja.Undefined(), vm.GlobalObject(), vm.ToValue(maxAddedGlobals))
	if err != nil {
		return nil, err
	}
	cleanup, ok := goja.AssertFunction(cleanupValue)
	if !ok {
		return nil, errors.New("the script isolation returned no cleanup")
	}
	return &scriptVM{vm: vm, cleanup: cleanup}, nil
}

// restore undoes what a run left in the global object and reports whether the vm
// may be used again.
func (this *scriptVM) restore() bool {
	clean, err := this.cleanup(goja.Undefined())
	return err == nil && clean.ToBoolean()
}

// maxEnvironmentVMs and maxProcessVMs cap the kept vms, ~0.5 MB each: 64 keep an
// environment the size of the Musterwerke (44 script channels) fully cached, and
// 256 bound the process at ~140 MB. A channel past either cap gets a fresh vm
// when it runs next; they are variables only so a test can lower them.
var (
	maxEnvironmentVMs = 64
	maxProcessVMs     = 256
)

// cachedVM is one channel's slot. The vm sits behind an atomic pointer because
// the process-wide eviction runs without the owning environment's lock: it only
// drops the pointer, and an owner that is mid-run keeps its own reference.
type cachedVM struct {
	current atomic.Pointer[scriptVM]
	local   *list.Element // in the environment's order, guarded by environment.mux
	process *list.Element // in processVMs.order, guarded by processVMs.mux
}

// processVMs orders every live kept vm, least recently used at the back. Its
// lock is only ever taken while an environment lock is held, never the reverse.
var processVMs = struct {
	mux   sync.Mutex
	order *list.List
}{order: list.New()}

// scriptVMs keeps the prepared vm of every script channel of one generation. It
// is guarded by environment.mux, the lock a run holds for its whole duration,
// so no two goroutines ever use one vm.
type scriptVMs struct {
	gen   *generation
	vms   map[*goja.Program]*cachedVM
	order *list.List // least recently used at the back
}

// take returns the vm of a program, preparing one if there is none; a program of
// another generation first discards every vm of the previous one.
func (this *scriptVMs) take(gen *generation, program *goja.Program) (*scriptVM, error) {
	if this.gen != gen {
		this.clear()
		this.gen = gen
	}
	slot := this.vms[program]
	if slot != nil {
		if prepared := slot.current.Load(); prepared != nil {
			this.order.MoveToFront(slot.local)
			processVMs.mux.Lock()
			if slot.process != nil {
				processVMs.order.MoveToFront(slot.process)
			}
			processVMs.mux.Unlock()
			return prepared, nil
		}
	}
	prepared, err := newScriptVM()
	if err != nil {
		return nil, err
	}
	if this.vms == nil {
		this.vms = map[*goja.Program]*cachedVM{}
		this.order = list.New()
	}
	if slot == nil {
		slot = &cachedVM{}
		this.vms[program] = slot
		slot.local = this.order.PushFront(program)
	} else {
		this.order.MoveToFront(slot.local)
	}
	slot.current.Store(prepared)
	for this.order.Len() > maxEnvironmentVMs {
		this.drop(this.order.Back().Value.(*goja.Program))
	}
	if this.vms[program] != slot {
		//the environment cap is zero and evicted this program at once: it is not
		//kept, so it never joins the process order
		return prepared, nil
	}
	processVMs.mux.Lock()
	if slot.process == nil {
		slot.process = processVMs.order.PushFront(slot)
	} else {
		processVMs.order.MoveToFront(slot.process)
	}
	for processVMs.order.Len() > maxProcessVMs {
		victim := processVMs.order.Remove(processVMs.order.Back()).(*cachedVM)
		victim.current.Store(nil)
		victim.process = nil
	}
	processVMs.mux.Unlock()
	return prepared, nil
}

// discard drops the vm of a program, so its next run starts from a fresh one.
func (this *scriptVMs) discard(program *goja.Program) {
	if _, ok := this.vms[program]; ok {
		this.drop(program)
	}
}

func (this *scriptVMs) drop(program *goja.Program) {
	slot := this.vms[program]
	slot.current.Store(nil)
	this.order.Remove(slot.local)
	delete(this.vms, program)
	processVMs.mux.Lock()
	if slot.process != nil {
		processVMs.order.Remove(slot.process)
		slot.process = nil
	}
	processVMs.mux.Unlock()
}

func (this *scriptVMs) clear() {
	for program := range this.vms {
		this.drop(program)
	}
	this.gen = nil
	this.vms = nil
	this.order = nil
}

// live returns the kept vm of a program without touching the order, for tests.
func (this *scriptVMs) live(program *goja.Program) *scriptVM {
	if slot := this.vms[program]; slot != nil {
		return slot.current.Load()
	}
	return nil
}
