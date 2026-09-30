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

// SPIKE (SNRGY-4817 variant C): channel scripts on QuickJS compiled to wasm
// (fastschema/qjs on wazero), selected with MOSES_SCRIPT_ENGINE=qjs. Like the
// goja path it keeps one prepared instance per channel program. Script and Go
// talk through one host function taking int arguments and one ArrayBuffer in
// linear memory, so a run crosses from Go into wasm exactly once.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
	"github.com/fastschema/qjs"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// engineQJS is read once: qjs compiles its wasm module on the first runtime and
// fixes CloseOnContextDone for the whole process at that moment.
var engineQJS = os.Getenv("MOSES_SCRIPT_ENGINE") == "qjs"

// qjsSettings are the spike's knobs, all from the environment.
type qjsSettings struct {
	patch       string // "none", "memory" or "stack" (memory cap plus stack check)
	timeout     string // "loop" (patched loop-header check), "context" (wazero CloseOnContextDone) or "off"
	maxPages    uint32
	memoryLimit int
}

func qjsSettingsFromEnv() qjsSettings {
	s := qjsSettings{patch: "stack", timeout: "loop", maxPages: 1024, memoryLimit: 32 << 20}
	if v := os.Getenv("MOSES_QJS_PATCH"); v != "" {
		s.patch = v
	}
	if v := os.Getenv("MOSES_QJS_TIMEOUT"); v != "" {
		s.timeout = v
	}
	if v, err := strconv.Atoi(os.Getenv("MOSES_QJS_MAX_MB")); err == nil && v > 0 {
		s.maxPages = uint32(v) * 16
	}
	return s
}

var qjsSetup = sync.OnceValues(func() (qjsEnvironment, error) {
	settings := qjsSettingsFromEnv()
	env := qjsEnvironment{settings: settings}
	if settings.patch != "none" || settings.timeout == "loop" {
		original, err := qjsOriginalWasm()
		if err != nil {
			return env, fmt.Errorf("reading qjs.wasm for the patch: %w", err)
		}
		patch := qjsPatch{checkStack: settings.patch == "stack", interrupt: settings.timeout == "loop"}
		if settings.patch != "none" {
			patch.maxPages = settings.maxPages
		}
		env.wasm, err = patchQJSWasm(original, patch)
		if err != nil {
			return env, fmt.Errorf("patching qjs.wasm: %w", err)
		}
	}
	//an empty directory, since qjs mounts its working directory as the guest root
	dir, err := os.MkdirTemp("", "moses-qjs-")
	if err != nil {
		return env, err
	}
	env.cwd = dir
	return env, nil
})

type qjsEnvironment struct {
	settings qjsSettings
	wasm     []byte
	cwd      string
}

// JSValue layout of the wasm32 build (NaN boxing), probed at instance start.
const (
	qjsTagInt       = 0
	qjsTagException = 6
	qjsUndefined    = uint64(3) << 32
	qjsHeaderBytes  = 16
)

// host ops, mirrored by the numbers in qjsPreludeSource
const (
	qjsOpIntern      = 1
	qjsOpGet         = 2
	qjsOpSetNum      = 3
	qjsOpSetStr      = 4
	qjsOpSetBool     = 5
	qjsOpSetJSON     = 6
	qjsOpSetNone     = 7
	qjsOpSetRefused  = 8
	qjsOpSendNum     = 9
	qjsOpSendStr     = 10
	qjsOpSendBool    = 11
	qjsOpSendJSON    = 12
	qjsOpSendNone    = 13
	qjsOpSendRefused = 14
	qjsOpGetRoom     = 15
	qjsOpGetDevice   = 16
	qjsOpConsole     = 17
	qjsOpHTTP        = 18
	qjsOpPending     = 19
	qjsOpRebind      = 20
	qjsOpFieldBad    = 21
	qjsOpInput       = 22
	qjsOpFmtNum      = 23
)

// result codes of a value-returning op
const (
	qjsResNum     = 0
	qjsResStr     = 1
	qjsResTrue    = 2
	qjsResFalse   = 3
	qjsResNull    = 4
	qjsResJSON    = 5
	qjsResPending = 6
	qjsResErr     = -1
	qjsResReset   = -3
)

// qjsMaxFields and qjsMaxScopes bound what one instance and one run keep on the
// Go side, whatever the script asks for.
const (
	qjsMaxFields = 4096
	qjsMaxScopes = 4096
)

var errQJSBridge = errors.New("the script bridge received a malformed request")

// qjsPreludeSource hardens the context, builds the moses api over the host
// function and returns [entry, setScript]; the isolation is the goja one.
const qjsPreludeSource = `(function (global, host, isolation, maxAdded) {
	'use strict';
	var ErrorT = Error, TypeErrorT = TypeError, RangeErrorT = RangeError;
	var ArrayBufferT = ArrayBuffer, Float64ArrayT = Float64Array, Int32ArrayT = Int32Array, Uint16ArrayT = Uint16Array;
	var MapT = Map, SetT = Set, StringT = String;
	var fromCharCode = String.fromCharCode, apply = Reflect.apply, isArray = Array.isArray, keysOf = Object.keys;
	var stringify = JSON.stringify, parse = JSON.parse, objectToString = Object.prototype.toString;
	var isInteger = Number.isInteger, finite = Number.isFinite, floor = Math.floor, abs = Math.abs, min = Math.min;
	var numberValueOf = Number.prototype.valueOf, booleanValueOf = Boolean.prototype.valueOf;
	var isView = ArrayBuffer.isView, nativeJoin = Array.prototype.join;
	var defineProperty = Object.defineProperty, describe = Object.getOwnPropertyDescriptor, prototypeOf = Object.getPrototypeOf;

	// qjs wraps the host in a rest-and-spread JavaScript function that costs ~3 us
	// a call; its inner C function is taken once through Function.prototype.call
	var functionProto = Function.prototype, nativeCall = functionProto.call, rawHost = null;
	functionProto.call = function (thisArg) { rawHost = this; return apply(nativeCall, this, arguments); };
	try { host(0, 0, 0); } finally { functionProto.call = nativeCall; }
	if (typeof rawHost !== 'function') throw new ErrorT('the script bridge could not reach the host function');
	host = function (op, a, b) { return rawHost(0, 0, 0, undefined, op, a, b); };

	var extras = ['std', 'os', 'bjson', 'setTimeout', 'setInterval', 'clearTimeout', 'clearInterval', 'print',
		'console', 'scriptArgs', 'navigator', 'queueMicrotask', 'performance', 'gc', '__moses_host'];
	for (var i = 0; i < extras.length; i++) delete global[extras[i]];

	function named(name, length, fn) {
		defineProperty(fn, 'name', {value: name, configurable: true});
		defineProperty(fn, 'length', {value: length, configurable: true});
		return fn;
	}
	function unavailable(what) {
		return function () { throw new TypeErrorT(what + ' is not available in MOSES scripts'); };
	}
	function replace(target, key, value) {
		var d = describe(target, key);
		defineProperty(target, key, {value: value, writable: d.writable, enumerable: d.enumerable, configurable: d.configurable});
	}
	replace(global, 'eval', named('eval', 1, unavailable('eval')));
	var constructors = [
		[prototypeOf(function () {}), 'Function'],
		[prototypeOf(function* () {}), 'GeneratorFunction'],
		[prototypeOf(async function () {}), 'AsyncFunction'],
		[prototypeOf(async function* () {}), 'AsyncGeneratorFunction']
	];
	for (i = 0; i < constructors.length; i++) {
		var proto = constructors[i][0], name = constructors[i][1];
		var blocked = named(name, 1, unavailable('the ' + name + ' constructor'));
		defineProperty(blocked, 'prototype', {value: proto, writable: false, enumerable: false, configurable: false});
		replace(proto, 'constructor', blocked);
		if (name === 'Function') replace(global, 'Function', blocked);
	}

	// the buffer: f64 slot at 0, int slots at 8 and 12, UTF-16 payload from 16
	var buf, f64, i32, u16, cap = 0, MAX_UNITS = 32 << 20;
	function bind(bytes) {
		//build the views before Go is told about the buffer: if a view allocation
		//throws, Go keeps the old address and JS keeps the old views, so the two
		//never end up on different buffers
		var b = new ArrayBufferT(bytes);
		var nf64 = new Float64ArrayT(b, 0, 1), ni32 = new Int32ArrayT(b, 0, 4), nu16 = new Uint16ArrayT(b);
		if (host(20, b, 0) !== 0) throw new ErrorT('the script bridge could not bind its buffer');
		buf = b; f64 = nf64; i32 = ni32; u16 = nu16;
		cap = (bytes - 16) >> 1;
	}
	function ensure(units) {
		if (units <= cap) return;
		if (units > MAX_UNITS) throw new RangeErrorT('the value is too large for the script bridge');
		var bytes = buf.byteLength;
		while (((bytes - 16) >> 1) < units) bytes *= 2;
		bind(bytes);
	}
	function putStr(s) {
		var n = s.length;
		ensure(n);
		for (var k = 0; k < n; k++) u16[8 + k] = s.charCodeAt(k);
		return n;
	}
	function getStr(n) {
		if (n <= 0) return '';
		if (n > cap) throw new ErrorT('the script bridge returned a malformed string');
		var parts = [], k = 0;
		while (k < n) {
			var end = min(n, k + 4096);
			parts[parts.length] = apply(fromCharCode, null, u16.subarray(8 + k, 8 + end));
			k = end;
		}
		return parts.length === 1 ? parts[0] : apply(nativeJoin, parts, ['']);
	}
	function result(code) {
		switch (code) {
			case 0: return f64[0];
			case 1: return getStr(i32[2]);
			case 2: return true;
			case 3: return false;
			case 4: return null;
			case 5: return parse(getStr(i32[2]));
			case 6: ensure(i32[2]); return result(host(19, 0, 0));
			default: throw new ErrorT(getStr(i32[2]));
		}
	}

	var fieldIds = new MapT();
	function fmtNumber(n) {
		f64[0] = n;
		return getStr(host(23, 0, 0));
	}
	// the key goja spelled: an integral number as int64, any other through Go's fmt
	function fieldId(f) {
		var key, t = typeof f;
		if (t === 'string') { if (f === '') return -1; key = f; }
		else if (t === 'number') key = (isInteger(f) && abs(f) < 9223372036854775808) ? StringT(f) : fmtNumber(f);
		else if (t === 'boolean') key = f ? 'true' : 'false';
		else return -1;
		var id = fieldIds.get(key);
		if (id !== undefined) return id;
		id = host(1, putStr(key), 0);
		if (id === -3) { fieldIds.clear(); id = host(1, putStr(key), 0); }
		if (id < 0) throw new ErrorT('the script bridge refused a field');
		fieldIds.set(key, id);
		return id;
	}

	function Refusal(message) { this.message = message; }
	var NOT_PLAIN = 'only numbers, strings, booleans, null and arrays or plain objects of those can be sent or stored';
	var MAX_BYTES = 16 << 20;
	// one bounded pass that reads every property once, as convertScriptValue does
	function plain(value, maxDepth, maxNodes, forState) {
		var nodes = 0, bytes = 0, seen = new SetT();
		function conv(v, depth) {
			nodes++;
			if (nodes > maxNodes) throw new Refusal('the value holds more than ' + maxNodes + ' elements');
			if (depth >= maxDepth) throw new Refusal('the value nests deeper than the ' + (maxDepth - 1) + ' level limit');
			if (v === undefined || v === null) return 'null';
			var t = typeof v;
			if (t === 'number') return finite(v) ? stringify(v) : 'null';
			if (t === 'string') {
				bytes += v.length;
				if (bytes > MAX_BYTES) throw new Refusal('the value holds more than ' + MAX_BYTES + ' bytes of strings');
				return stringify(v);
			}
			if (t === 'boolean') return v ? 'true' : 'false';
			if (t !== 'object') throw new Refusal(NOT_PLAIN + ', not ' + t);
			if (seen.has(v)) throw new Refusal('the value refers to itself');
			seen.add(v);
			try {
				var tag = apply(objectToString, v, []), parts = [], k;
				if (isArray(v) || tag === '[object Arguments]') {
					var len = v.length;
					len = len > 0 ? floor(len) : 0;
					for (k = 0; k < len; k++) parts[parts.length] = conv(v[k], depth + 1);
					return '[' + apply(nativeJoin, parts, [',']) + ']';
				}
				if (tag === '[object Number]') { try { var n = apply(numberValueOf, v, []); return finite(n) ? stringify(n) : 'null'; } catch (e) {} }
				if (tag === '[object Boolean]') { try { return apply(booleanValueOf, v, []) ? 'true' : 'false'; } catch (e) {} }
				if (tag === '[object Date]' || tag === '[object Map]' || tag === '[object Set]' || tag === '[object WeakMap]' ||
					tag === '[object WeakSet]' || tag === '[object ArrayBuffer]' || tag === '[object RegExp]' ||
					tag === '[object Promise]' || tag === '[object BigInt]' || tag === '[object Symbol]' || isView(v)) {
					throw new Refusal(NOT_PLAIN);
				}
				var keys = keysOf(v);
				for (k = 0; k < keys.length; k++) {
					bytes += keys[k].length;
					if (bytes > MAX_BYTES) throw new Refusal('the value holds more than ' + MAX_BYTES + ' bytes of strings');
					parts[parts.length] = stringify(keys[k]) + ':' + conv(v[keys[k]], depth + 1);
				}
				return '{' + apply(nativeJoin, parts, [',']) + '}';
			} finally {
				seen.delete(v);
			}
		}
		return conv(value, 0);
	}

	function stateApi(sid) {
		var base = sid << 8;
		return {
			get: function (field) {
				var id = fieldId(field);
				if (id < 0) { host(21, 0, 0); return 0; }
				return result(host(base | 2, id, 0));
			},
			set: function (field, value) {
				var id = fieldId(field);
				if (id < 0) { host(21, 0, 0); return; }
				var t = typeof value, r;
				if (t === 'number') { f64[0] = value; r = host(base | 3, id, 0); }
				else if (t === 'string') r = host(base | 4, id, putStr(value));
				else if (t === 'boolean') r = host(base | 5, id, value ? 1 : 0);
				else if (value === undefined || value === null) r = host(base | 7, id, 0);
				else {
					var text, refusal;
					try { text = plain(value, 33, 10000, true); }
					catch (e) { if (e instanceof Refusal) refusal = e.message; else throw e; }
					r = refusal !== undefined ? host(base | 8, id, putStr(refusal)) : host(base | 6, id, putStr(text));
				}
				if (r < 0) throw new ErrorT(getStr(i32[2]));
			}
		};
	}
	function send(value) {
		var t = typeof value;
		if (t === 'number') { f64[0] = value; host(9, 0, 0); return; }
		if (value === undefined || value === null) { host(13, 0, 0); return; }
		if (t === 'string') { host(10, putStr(value), 0); return; }
		if (t === 'boolean') { host(11, value ? 1 : 0, 0); return; }
		var text;
		try { text = plain(value, 1000, 1048576, false); }
		catch (e) { if (e instanceof Refusal) { host(14, putStr(e.message), 0); return; } throw e; }
		host(12, putStr(text), 0);
	}
	function idOf(value) {
		return value === undefined || value === null ? '' : StringT(value);
	}
	function zoneApi(sid) {
		return {
			state: stateApi(sid),
			getDevice: function (assetId) {
				var s = host((sid << 8) | 16, putStr(idOf(assetId)), 0);
				return s < 0 ? {} : {state: stateApi(s)};
			}
		};
	}
	function mosesApi(input) {
		var environment = {
			state: stateApi(0),
			getRoom: function (zoneId) {
				var s = host(15, putStr(idOf(zoneId)), 0);
				return s < 0 ? {} : zoneApi(s);
			}
		};
		var zone = zoneApi(1), asset = {state: stateApi(2)}, channel = {input: input, send: send};
		return {world: environment, room: zone, device: asset, service: channel,
			environment: environment, zone: zone, asset: asset, channel: channel};
	}
	function consoleApi(flags) {
		function emit(level, enabled, args) {
			if (!enabled) return;
			var parts = [];
			for (var k = 0; k < args.length; k++) {
				try { parts[parts.length] = plain(args[k], 1000, 1048576, false); }
				catch (e) {
					if (!(e instanceof Refusal)) throw e;
					parts[parts.length] = stringify('[unloggable value: ' + e.message + ']');
				}
			}
			host((level << 8) | 17, putStr('[' + apply(nativeJoin, parts, [',']) + ']'), 0);
		}
		var debug = function () { emit(0, flags & 1, arguments); };
		var warn = function () { emit(1, flags & 2, arguments); };
		return {log: debug, info: debug, debug: debug, warn: warn, error: warn};
	}
	function httpGet(endpoint) {
		return result(host(18, putStr(idOf(endpoint)), 0));
	}

	var script = null;
	function setScript(fn) { script = fn; }
	function describeError(e) {
		var text = StringT(e);
		if (e !== null && typeof e === 'object' && typeof e.stack === 'string' && e.stack !== '') text += '\n' + e.stack;
		return text.length > 2000 ? text.substring(0, 2000) : text;
	}
	function entry() {
		var flags = i32[2];
		var input = (flags & 4) ? result(host(22, 0, 0)) : null;
		var api = mosesApi(input), con = consoleApi(flags);
		global.moses = api; global.httpGet = httpGet; global.console = con;
		var status = 0;
		try {
			apply(script, global, [api, httpGet, con]);
		} catch (e) {
			status = 2;
			var message;
			try { message = describeError(e); } catch (e2) { message = 'the script threw a value that cannot be described'; }
			try { i32[2] = putStr(message); } catch (e3) { i32[2] = 0; }
		}
		var clean = false;
		try { clean = cleanup(); } catch (e) {}
		return status | (clean ? 0 : 1);
	}

	bind(65536);
	var cleanup = isolation(global, maxAdded);
	return [entry, setScript];
})`

// qjsImport finds the import keyword, which dynamic import() would turn into a
// way to the qjs:std and qjs:os modules; strings and comments count too.
var qjsImport = regexp.MustCompile(`\bimport\b`)

// qjsVM is one prepared QuickJS instance and the Go end of its bridge.
type qjsVM struct {
	rt        *qjs.Runtime
	mem       api.Memory
	callFn    api.Function // QJS_Call, kept so its grown wasm stack is reused
	ctxRaw    uint64
	entry     uint64
	bufAddr   uint32
	bufLen    uint32
	fields    []string
	run       *qjsRun
	pending   []uint16
	pendCode  int32
	dead      bool
	reusable  bool
	loopStop  bool // the timeout sets the interrupt word instead of closing the module
	prepareAt time.Duration
}

// qjsRun is what one run's host calls resolve against.
type qjsRun struct {
	rt      *Runtime
	env     *environment
	gen     *generation
	binding channelBinding
	send    func(value interface{})
	now     time.Time
	ctx     context.Context
	input   interface{}
	scopes  []qjsScope
}

type qjsScope struct {
	kind byte // 'c' context, 'z' zone, 'a' asset
	id   string
}

func newQJSVM(binding channelBinding) (vm *qjsVM, err error) {
	setup, err := qjsSetup()
	if err != nil {
		return nil, err
	}
	if qjsImport.MatchString(binding.code) {
		return nil, errors.New("import is not available in MOSES scripts")
	}
	started := time.Now()
	vm = &qjsVM{}
	defer func() {
		if p := recover(); p != nil {
			vm.close()
			vm, err = nil, qjsCallError(p)
		}
	}()
	rt, err := qjs.New(qjs.Option{
		CWD:                setup.cwd,
		Context:            context.Background(),
		CloseOnContextDone: setup.settings.timeout == "context",
		MemoryLimit:        setup.settings.memoryLimit,
		ProxyFunction:      vm.host,
		Stdout:             io.Discard,
		Stderr:             io.Discard,
		QuickJSWasmBytes:   setup.wasm,
	})
	if err != nil {
		return nil, err
	}
	vm.rt = rt
	vm.loopStop = setup.settings.timeout == "loop"
	vm.ctxRaw = rt.Context().Raw()
	if raw := rt.Call("QJS_NewInt32", vm.ctxRaw, uint64(uint32(0xfffffffb))).Raw(); raw != 0xfffffffb {
		vm.close()
		return nil, fmt.Errorf("unexpected JSValue layout %#x", raw)
	}
	hostFn := rt.Context().Function(func(*qjs.This) (*qjs.Value, error) { return nil, nil })
	rt.Context().Global().SetPropertyStr("__moses_host", hostFn)
	prelude := qjsPreludeSource + "(globalThis, globalThis.__moses_host, " + isolationSource + ", " + strconv.Itoa(maxAddedGlobals) + ")"
	parts, err := vm.eval("moses-prelude", prelude)
	if err != nil {
		vm.close()
		return nil, err
	}
	vm.entry = rt.Call("QJS_GetPropertyUint32", vm.ctxRaw, parts, 0).Raw()
	setScript := rt.Call("QJS_GetPropertyUint32", vm.ctxRaw, parts, 1).Raw()
	vm.free(parts)

	program, err := goja.Parse(binding.channel.Id, binding.code, parser.WithDisableSourceMaps)
	if err != nil {
		vm.close()
		return nil, err
	}
	var fn uint64
	for _, wrapped := range qjsWrapScript(binding.code, topLevelLexicalNames(program)) {
		if fn, err = vm.eval(binding.channel.Id, wrapped); err == nil {
			break
		}
	}
	if err != nil {
		vm.free(setScript)
		vm.close()
		return nil, err
	}
	//the argument array lives in the bridge buffer, which the prelude has bound
	if !vm.mem.WriteUint64Le(vm.bufAddr, fn) {
		vm.close()
		return nil, errQJSBridge
	}
	if raw := rt.Call("QJS_Call", vm.ctxRaw, setScript, qjsUndefined, 1, uint64(vm.bufAddr)).Raw(); raw>>32 == qjsTagException {
		vm.close()
		return nil, errors.New(vm.exception())
	}
	vm.free(fn)
	vm.free(setScript)
	vm.prepareAt = time.Since(started)
	qjsCounters.prepares.Add(1)
	return vm, nil
}

// qjsWrapScript is wrapScript without the call: evaluating it yields the function
// a run calls with the api.
func qjsWrapScript(code string, shadowed map[string]bool) []string {
	var out []string
	for _, wrapped := range wrapScript(code, shadowed) {
		out = append(out, wrapped[:strings.LastIndex(wrapped, "\n}).call(this")]+"\n})")
	}
	return out
}

func (this *qjsVM) free(value uint64) {
	//negative tags are the reference-counted values
	if int32(uint32(value>>32)) < 0 {
		this.rt.Call("JS_FreeValue", this.ctxRaw, value)
	}
}

// eval evaluates code as a sloppy global script, which qjs's own Eval cannot:
// it always adds the strict flag.
func (this *qjsVM) eval(name string, code string) (uint64, error) {
	codePtr := this.cString(code)
	namePtr := this.cString(name)
	defer this.rt.FreeHandle(codePtr)
	defer this.rt.FreeHandle(namePtr)
	opts := this.rt.Call("QJS_CreateEvalOption", codePtr, 0, 0, namePtr, 0).Raw()
	defer this.rt.FreeHandle(opts)
	raw := this.rt.Call("QJS_Eval", this.ctxRaw, opts).Raw()
	if raw>>32 == qjsTagException {
		return 0, errors.New(this.exception())
	}
	return raw, nil
}

func (this *qjsVM) cString(s string) uint64 {
	ptr := this.rt.Malloc(uint64(len(s) + 1))
	this.rt.Mem().MustWrite(uint32(ptr), append([]byte(s), 0))
	return ptr
}

// exception takes the pending exception as text.
func (this *qjsVM) exception() string {
	exc := this.rt.Call("JS_GetException", this.ctxRaw).Raw()
	defer this.free(exc)
	packed := this.rt.Call("QJS_ToCString", this.ctxRaw, exc).Raw()
	if packed == 0 {
		return "the script threw a value that cannot be described"
	}
	defer this.rt.FreeHandle(packed)
	word, ok := this.rt.Mem().ReadUint64(uint32(packed))
	if ok != nil {
		return "the script threw"
	}
	addr, size := uint32(word>>32), uint32(word)
	text, err := this.rt.Mem().Read(addr, uint64(size))
	message := string(text)
	if err != nil {
		message = "the script threw"
	}
	this.rt.Call("JS_FreeCString", this.ctxRaw, uint64(addr))
	return message
}

func (this *qjsVM) close() {
	if this == nil || this.rt == nil {
		return
	}
	defer func() { _ = recover() }()
	//a loop-mode timeout leaves the interrupt word set; clear it so the guest code
	//that freeing the runtime runs does not trap on the first loop header
	if this.loopStop && this.mem != nil {
		this.mem.WriteUint32Le(qjsInterruptAddr, 0)
	}
	//a trapped instance may hold a half-updated heap; skip freeing the entry value
	//and let Close tear the whole module down
	if !this.dead && this.entry != 0 {
		this.free(this.entry)
	}
	this.rt.Close()
}

// qjsCallError turns a panic out of a qjs call into the error a run reports.
func qjsCallError(p interface{}) error {
	err, ok := p.(error)
	if !ok {
		err = fmt.Errorf("%v", p)
	}
	var exit *sys.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == sys.ExitCodeDeadlineExceeded || exit.ExitCode() == sys.ExitCodeContextCanceled) {
		return ErrScriptTimeout
	}
	text := err.Error()
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return fmt.Errorf("the script engine trapped: %s", strings.TrimPrefix(text, "failed to call "))
}

// runOnce runs the channel script once; the caller holds the environment lock.
func (this *qjsVM) runOnce(run *qjsRun, timeout time.Duration) (err error) {
	flags := uint32(0)
	if util.Logger.Enabled(context.Background(), slog.LevelDebug) {
		flags |= 1
	}
	if util.Logger.Enabled(context.Background(), slog.LevelWarn) {
		flags |= 2
	}
	if run.input != nil {
		flags |= 4
	}
	if !this.mem.WriteUint32Le(this.bufAddr+8, flags) {
		this.dead = true
		return errQJSBridge
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	run.ctx = ctx
	run.scopes = []qjsScope{{kind: 'c'}, {kind: 'z', id: run.binding.zoneId}, {kind: 'a', id: run.binding.asset.id}}
	jsCtx := this.rt.Context()
	if !this.loopStop {
		jsCtx.Context = ctx
	}
	var timer *time.Timer
	fired := make(chan struct{})
	if this.loopStop {
		mem := this.mem
		if !mem.WriteUint32Le(qjsInterruptAddr, 0) {
			this.dead = true
			return errQJSBridge
		}
		timer = time.AfterFunc(timeout, func() {
			mem.WriteUint32Le(qjsInterruptAddr, 1)
			close(fired)
		})
	}
	this.run = run
	defer func() {
		late := false
		if timer != nil && !timer.Stop() {
			//the callback has run or is running: wait for its write, so it cannot land
			//in a later run, and do not keep an instance it may have stopped
			<-fired
			late = true
		}
		jsCtx.Context = context.Background()
		this.run = nil
		this.pending = nil
		if p := recover(); p != nil {
			this.dead = true
			err = qjsCallError(p)
			if late {
				err = ErrScriptTimeout
			}
		} else if late {
			this.reusable = false
		}
	}()
	this.reusable = false
	results, callErr := this.callFn.Call(jsCtx, this.ctxRaw, this.entry, qjsUndefined, 0, 0)
	if callErr != nil {
		panic(callErr)
	}
	raw := results[0]
	switch uint32(raw >> 32) {
	case qjsTagInt:
		status := int32(uint32(raw))
		this.reusable = status&1 == 0
		if status&2 != 0 {
			units, _ := this.mem.ReadUint32Le(this.bufAddr + 8)
			message, readErr := this.readStr(int(int32(units)))
			if readErr != nil {
				message = "the script threw"
			}
			if strings.Contains(message, "out of memory") {
				//the linear memory stays at its high-water mark otherwise
				this.reusable = false
			}
			return errors.New(message)
		}
		return nil
	case qjsTagException:
		return errors.New(this.exception())
	default:
		this.free(raw)
		return errors.New("the script entry returned an unexpected value")
	}
}

// qjsCounters count runs and host calls for the profile, spike only. They are
// atomic because environments update them under their own separate mutexes.
var qjsCounters struct{ runs, hostCalls, prepares atomic.Int64 }

// host is the one function the script side calls: host(op | sid<<8, a, b).
func (this *qjsVM) host(ctx context.Context, module api.Module, _ uint32, _ uint64, argc uint32, argv uint32) uint64 {
	qjsCounters.hostCalls.Add(1)
	mem := module.Memory()
	arg := func(i uint32) (uint64, bool) {
		if i >= argc {
			return qjsUndefined, true
		}
		return mem.ReadUint64Le(argv + i*8)
	}
	intArg := func(i uint32) (int32, bool) {
		v, ok := arg(i)
		if !ok || uint32(v>>32) != qjsTagInt {
			return 0, false
		}
		return int32(uint32(v)), true
	}
	code, ok := intArg(4)
	if !ok {
		return this.fail(mem, errQJSBridge)
	}
	op, sid := code&0xff, int(code>>8)
	if op == 0 {
		return 0
	}
	if op == qjsOpRebind {
		value, ok := arg(5)
		if !ok {
			return this.fail(mem, errQJSBridge)
		}
		return qjsInt(this.rebind(ctx, module, value))
	}
	a, okA := intArg(5)
	b, okB := intArg(6)
	if !okA || !okB {
		return this.fail(mem, errQJSBridge)
	}
	run := this.run
	if run == nil {
		return this.fail(mem, errors.New("the script bridge is not in a run"))
	}
	result, err := this.dispatch(run, mem, op, sid, a, b)
	if err != nil {
		return this.fail(mem, err)
	}
	return qjsInt(result)
}

func qjsInt(v int32) uint64 {
	return uint64(uint32(v))
}

func (this *qjsVM) rebind(ctx context.Context, module api.Module, buffer uint64) int32 {
	this.mem = module.Memory()
	if this.callFn == nil {
		this.callFn = module.ExportedFunction("QJS_Call")
	}
	fn := module.ExportedFunction("QJS_GetArrayBuffer")
	results, err := fn.Call(ctx, this.ctxRaw, buffer)
	if err != nil || len(results) == 0 || results[0] == 0 {
		return qjsResErr
	}
	packed := uint32(results[0])
	word, ok := this.mem.ReadUint64Le(packed)
	if free := module.ExportedFunction("free"); free != nil {
		_, _ = free.Call(ctx, uint64(packed))
	}
	if !ok {
		return qjsResErr
	}
	addr, size := uint32(word>>32), uint32(word)
	if size < 64 {
		return qjsResErr
	}
	if _, ok := this.mem.Read(addr, size); !ok {
		return qjsResErr
	}
	this.bufAddr, this.bufLen = addr, size
	return 0
}

// fail hands an error text to the script side, which throws it.
func (this *qjsVM) fail(mem api.Memory, err error) uint64 {
	units := utf16.Encode([]rune(err.Error()))
	room := int(this.bufLen-qjsHeaderBytes) / 2
	if len(units) > room {
		units = units[:room]
	}
	this.writeUnits(mem, units)
	return qjsInt(qjsResErr)
}

func (this *qjsVM) writeUnits(mem api.Memory, units []uint16) bool {
	raw := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(raw[2*i:], u)
	}
	return mem.WriteUint32Le(this.bufAddr+8, uint32(len(units))) && mem.Write(this.bufAddr+qjsHeaderBytes, raw)
}

func (this *qjsVM) readStr(units int) (string, error) {
	if units == 0 {
		return "", nil
	}
	if units < 0 || qjsHeaderBytes+uint64(units)*2 > uint64(this.bufLen) {
		return "", errQJSBridge
	}
	raw, ok := this.mem.Read(this.bufAddr+qjsHeaderBytes, uint32(units)*2)
	if !ok {
		return "", errQJSBridge
	}
	u := make([]uint16, units)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return string(utf16.Decode(u)), nil
}

// result writes a string result, or keeps it pending when it does not fit.
func (this *qjsVM) resultStr(mem api.Memory, code int32, s string) int32 {
	units := utf16.Encode([]rune(s))
	if qjsHeaderBytes+2*len(units) > int(this.bufLen) {
		this.pending, this.pendCode = units, code
		mem.WriteUint32Le(this.bufAddr+8, uint32(len(units)))
		return qjsResPending
	}
	if !this.writeUnits(mem, units) {
		return qjsResErr
	}
	return code
}

func (this *qjsVM) resultValue(mem api.Memory, value interface{}) (int32, error) {
	switch v := value.(type) {
	case nil:
		return qjsResNull, nil
	case bool:
		if v {
			return qjsResTrue, nil
		}
		return qjsResFalse, nil
	case string:
		return this.resultStr(mem, qjsResStr, v), nil
	}
	if number, ok := asFloat(value); ok {
		mem.WriteFloat64Le(this.bufAddr, number)
		return qjsResNum, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return this.resultStr(mem, qjsResJSON, string(encoded)), nil
}

func (this *qjsVM) dispatch(run *qjsRun, mem api.Memory, op int32, sid int, a int32, b int32) (int32, error) {
	env := run.env
	scope := func() (qjsScope, error) {
		if sid < 0 || sid >= len(run.scopes) {
			return qjsScope{}, errQJSBridge
		}
		return run.scopes[sid], nil
	}
	field := func() (string, error) {
		if a < 0 || int(a) >= len(this.fields) {
			return "", errQJSBridge
		}
		return this.fields[a], nil
	}
	str := func(units int32) (string, error) { return this.readStr(int(units)) }
	switch op {
	case qjsOpIntern:
		name, err := str(a)
		if err != nil {
			return 0, err
		}
		if len(this.fields) >= qjsMaxFields {
			this.fields = nil
			return qjsResReset, nil
		}
		this.fields = append(this.fields, name)
		return int32(len(this.fields) - 1), nil
	case qjsOpFmtNum:
		number, _ := mem.ReadFloat64Le(this.bufAddr)
		units := utf16.Encode([]rune(fmt.Sprint(number)))
		this.writeUnits(mem, units)
		return int32(len(units)), nil
	case qjsOpFieldBad:
		env.warnNoField()
		return 0, nil
	case qjsOpGet:
		sc, err := scope()
		if err != nil {
			return 0, err
		}
		name, err := field()
		if err != nil {
			return 0, err
		}
		return this.resultValue(mem, run.stateGet(sc, name))
	case qjsOpSetNum, qjsOpSetStr, qjsOpSetBool, qjsOpSetJSON, qjsOpSetNone, qjsOpSetRefused:
		sc, err := scope()
		if err != nil {
			return 0, err
		}
		name, err := field()
		if err != nil {
			return 0, err
		}
		if run.governed(sc, name) {
			env.warnTimelineGoverned(name)
			return 0, nil
		}
		var value interface{}
		switch op {
		case qjsOpSetNone:
			util.Logger.Warn("the script handed no value", "environment", env.id, "field", name)
			return 0, nil
		case qjsOpSetNum:
			value, _ = mem.ReadFloat64Le(this.bufAddr)
		case qjsOpSetBool:
			value = b != 0
		case qjsOpSetStr:
			if value, err = str(b); err != nil {
				return 0, err
			}
		case qjsOpSetRefused:
			message, err := str(b)
			if err != nil {
				return 0, err
			}
			return 0, fmt.Errorf("state %q: %s", name, message)
		case qjsOpSetJSON:
			text, err := str(b)
			if err != nil {
				return 0, err
			}
			if err := json.Unmarshal([]byte(text), &value); err != nil {
				return 0, fmt.Errorf("state %q: %w", name, err)
			}
		}
		copied, err := jsguard.CopyPlainData(value)
		if err != nil {
			return 0, fmt.Errorf("state %q: %w", name, err)
		}
		run.states(sc)[name] = jsNumber(copied)
		env.dirty = true
		return 0, nil
	case qjsOpSendNum:
		number, _ := mem.ReadFloat64Le(this.bufAddr)
		run.send(number)
		return 0, nil
	case qjsOpSendStr:
		text, err := str(a)
		if err != nil {
			return 0, err
		}
		run.send(text)
		return 0, nil
	case qjsOpSendBool:
		run.send(a != 0)
		return 0, nil
	case qjsOpSendNone:
		util.Logger.Warn("the script handed no value", "environment", env.id, "field", "send")
		return 0, nil
	case qjsOpSendRefused:
		message, err := str(a)
		if err != nil {
			return 0, err
		}
		util.Logger.Warn("the script handed a value that cannot be published, it is dropped",
			attributes.ErrorKey, message, "environment", env.id, "field", "send")
		return 0, nil
	case qjsOpSendJSON:
		text, err := str(a)
		if err != nil {
			return 0, err
		}
		var value interface{}
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			return 0, err
		}
		run.send(jsNumber(value))
		return 0, nil
	case qjsOpGetRoom:
		zoneId, err := str(a)
		if err != nil {
			return 0, err
		}
		if _, known := run.gen.zones[zoneId]; !known {
			util.Logger.Warn("no zone for id found", "environment", env.id, "id", zoneId)
			return -1, nil
		}
		return run.addScope(qjsScope{kind: 'z', id: zoneId})
	case qjsOpGetDevice:
		sc, err := scope()
		if err != nil || sc.kind != 'z' {
			return 0, errQJSBridge
		}
		assetId, err := str(a)
		if err != nil {
			return 0, err
		}
		asset, known := run.gen.assets[assetId]
		if !known || asset.zoneId != sc.id {
			util.Logger.Warn("no asset for id found in this zone", "environment", env.id, "zone", sc.id, "id", assetId)
			return -1, nil
		}
		return run.addScope(qjsScope{kind: 'a', id: assetId})
	case qjsOpConsole:
		text, err := str(a)
		if err != nil {
			return 0, err
		}
		var args []interface{}
		if err := json.Unmarshal([]byte(text), &args); err != nil {
			return 0, err
		}
		var sb strings.Builder
		for i, value := range args {
			if sb.Len() >= maxConsoleBytes {
				sb.WriteString(" [truncated]")
				break
			}
			if i > 0 {
				sb.WriteByte(' ')
			}
			writeBounded(&sb, value, maxConsoleBytes)
		}
		if sid == 0 {
			util.Logger.Debug("script console", "arguments", sb.String())
		} else {
			util.Logger.Warn("script console", "arguments", sb.String())
		}
		return 0, nil
	case qjsOpHTTP:
		endpoint, err := str(a)
		if err != nil {
			return 0, err
		}
		return this.resultStr(mem, qjsResStr, qjsHTTPGet(run.ctx, endpoint)), nil
	case qjsOpInput:
		encoded, err := json.Marshal(run.input)
		if err != nil {
			return qjsResNull, nil
		}
		return this.resultStr(mem, qjsResJSON, string(encoded)), nil
	case qjsOpPending:
		units, code := this.pending, this.pendCode
		this.pending = nil
		if qjsHeaderBytes+2*len(units) > int(this.bufLen) || !this.writeUnits(mem, units) {
			return 0, errQJSBridge
		}
		return code, nil
	}
	return 0, errQJSBridge
}

func (this *qjsRun) addScope(scope qjsScope) (int32, error) {
	if len(this.scopes) >= qjsMaxScopes {
		return 0, errors.New("the script asked for too many zones and assets in one run")
	}
	this.scopes = append(this.scopes, scope)
	return int32(len(this.scopes) - 1), nil
}

func (this *qjsRun) states(scope qjsScope) map[string]interface{} {
	switch scope.kind {
	case 'z':
		this.env.advanceZone(scope.id, this.now)
		return this.env.zoneStates(scope.id)
	case 'a':
		return this.env.assetStates(scope.id)
	}
	return this.env.contextStates()
}

func (this *qjsRun) governed(scope qjsScope, name string) bool {
	return scope.kind == 'c' && this.gen != nil && this.gen.timeline != nil && this.gen.timeline.governsContext(name)
}

// stateGet is jsStateApi's and jsContextStateApi's get after the field check.
func (this *qjsRun) stateGet(scope qjsScope, name string) interface{} {
	env := this.env
	if this.governed(scope, name) {
		if value, governed := this.gen.timeline.effectiveContext(name, this.now); governed {
			return readState(env, name, value)
		}
		if value, exists := env.contextStates()[name]; exists {
			return readState(env, name, value)
		}
		return 0
	}
	target := this.states(scope)
	value, ok := target[name]
	if !ok {
		target[name] = 0
		env.dirty = true
		return 0
	}
	return readState(env, name, value)
}

// qjsHTTPGet is httpGet bound to the run's deadline, so a hanging endpoint ends
// with the run instead of holding the environment lock.
func qjsHTTPGet(ctx context.Context, endpoint string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		util.Logger.Warn("httpGet failed", attributes.ErrorKey, err, "endpoint", endpoint)
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		util.Logger.Warn("httpGet failed", attributes.ErrorKey, err, "endpoint", endpoint)
		return ""
	}
	defer resp.Body.Close()
	temp, err := io.ReadAll(resp.Body)
	if err != nil {
		util.Logger.Warn("httpGet unable to read response body", attributes.ErrorKey, err, "endpoint", endpoint)
		return ""
	}
	return string(temp)
}

// qjsVMs keeps the prepared instance of every script channel of a generation,
// guarded by environment.mux like scriptVMs; a failed preparation is kept too,
// so a channel that cannot start does not rebuild an instance on every run.
type qjsVMs struct {
	gen    *generation
	vms    map[*goja.Program]*qjsVM
	failed map[*goja.Program]error
}

func (this *qjsVMs) take(gen *generation, binding channelBinding) (*qjsVM, error) {
	if this.gen != gen {
		this.clear()
		this.gen = gen
		this.vms = map[*goja.Program]*qjsVM{}
		this.failed = map[*goja.Program]error{}
	}
	if vm := this.vms[binding.script]; vm != nil {
		return vm, nil
	}
	if err := this.failed[binding.script]; err != nil {
		return nil, err
	}
	vm, err := newQJSVM(binding)
	if err != nil {
		this.failed[binding.script] = err
		return nil, err
	}
	if len(this.vms) >= maxEnvironmentVMs {
		for program, victim := range this.vms {
			victim.close()
			delete(this.vms, program)
			break
		}
	}
	this.vms[binding.script] = vm
	return vm, nil
}

func (this *qjsVMs) discard(program *goja.Program) {
	if vm := this.vms[program]; vm != nil {
		vm.close()
		delete(this.vms, program)
	}
}

func (this *qjsVMs) clear() {
	for _, vm := range this.vms {
		vm.close()
	}
	this.gen, this.vms, this.failed = nil, nil, nil
}

// pages reports the wasm linear memory the kept instances hold.
func (this *qjsVMs) pages() (instances int, bytes uint64) {
	for _, vm := range this.vms {
		instances++
		bytes += uint64(vm.mem.Size())
	}
	return instances, bytes
}

// executeQJS is execute's QuickJS path: same lock, same crash brake, same
// failure report, an instance kept per channel program.
func (this *Runtime) executeQJS(env *environment, gen *generation, binding channelBinding, input interface{}, send func(value interface{}), now time.Time) error {
	env.mux.Lock()
	defer env.mux.Unlock()
	if this.brake != nil {
		defer this.brake.Enter(env.id, binding.channel.Id)()
	}
	vm, err := env.scripts.qjs.take(gen, binding)
	if err != nil {
		return err
	}
	qjsCounters.runs.Add(1)
	run := &qjsRun{rt: this, env: env, gen: gen, binding: binding, send: send, now: now, input: input}
	err = vm.runOnce(run, this.jsTimeout)
	if vm.dead || !vm.reusable {
		env.scripts.qjs.discard(binding.script)
	}
	return err
}
