# Script and request-body limits

## Scope

**Applies when** writing, reviewing or debugging what a channel script
(`Source.Script.Code`), a legacy routine or service script, or an API request
body may contain. **Delimitation:** the neighbouring case is the script
*timeout* (`JS_TIMEOUT`), which bounds how long a script runs. The limits here
bound what the JavaScript engines are handed to parse or compile, which the
timeout cannot interrupt.

## Why

goja's and otto's parsers, otto's evaluator and goja's JSON and regexp code
recurse on the Go stack. A Go stack overflow is a fatal error that no recover
catches: the process dies, and restarts into the same stored script. A trailing
`//# sourceMappingURL=file://...` comment also made goja read the named file,
so `file:///dev/zero` was an unbounded read.

Some native paths recurse over a value the script builds at run time rather than
over the source, so the size and nesting limits below do not bound them; they are
guarded per run instead. A backtracking regexp on goja's regexp2 engine ignores
the script interrupt, so it is bounded by a match timeout. What no in-process
guard can catch, the crash brake turns from an endless crash loop into a single
crash.

## What is guarded

Before parsing (`lib/jsguard`, applied by validation, by the runtime before each
compile and by the legacy runner before each run):

- **Size:** 32 KiB per script. This is the hard bound. goja's parser uses at
  most ~2.1 KB of stack per source byte (unclosed `(` or `[`; objects ~0.8 KB,
  every non-bracket chain such as `!!!…`, `x=>x=>…`, `a?b:…`, `a=a=…`, `**` or
  `if(1)if(1)…` at most ~0.16 KB). The worst 32 KiB script's parse needs the
  goroutine stack to grow to 128 MB (measured: it parses under a 128 MB cap and
  overflows a 127 MB one; Go's high-water sits below that, the allocation jumps
  in powers of two). The process caps the goroutine stack at 256 MB
  (`jsguard.MaxGoroutineStack`, `debug.SetMaxStack`), one full doubling of
  headroom over that worst script, so a runaway recursion overflows fatally, with
  a crash report, well under the pod's ~1 GiB memory limit rather than being
  OOM-killed without one. The largest real script (Musterwerke) is 2948 bytes.
- **Nesting depth:** 200 levels of `()`, `[]`, `{}`, template substitutions and
  regexp groups inside regexp literals. The deepest real script nests 5 levels.
  The scan lexes strings, templates, comments and regexp literals the way goja
  does. Where a `/` could be a regexp or a division (after `}`, `of`, `yield`,
  `await`), it counts every bracket in that span. It is a precision layer for
  clear messages, not the guarantee: a crafted script that makes it lose track
  is still bounded by the size limit.

In every script run (`lib/runtime/jsharden.go`, `lib/state/jsharden.go`):

- `eval`, `Function` and every other function constructor
  (`(function(){}).constructor`, generator and async function constructors) are
  **unavailable**. A call throws `TypeError: ... is not available in MOSES scripts`.
  No known script uses them.
- `JSON.parse` refuses text nesting deeper than 1000 levels (goja; otto's
  decoder already stops at 10000).
- `RegExp`, `RegExp.prototype.compile`, and `String.prototype.match`,
  `matchAll` and `search` with a non-RegExp argument check the pattern: at most
  32 KiB and 200 levels of groups. `RegExp.prototype[Symbol.split]` and
  `[Symbol.matchAll]` only accept a real RegExp as receiver, since they build a
  new one from it. Each untrusted value is converted to a string once, and that
  string is what the engine compiles.
- Nested JavaScript calls are limited to 1000 levels. goja ends the run with an
  uncatchable error, otto with an error. This bounds recursion that passes
  through a JavaScript function (`forEach` and `sort` callbacks, getters,
  revivers, Proxy traps) and, on otto, plain recursion. It does not bound native
  recursion that calls no JavaScript function, such as a chain of Proxies without
  traps (see Residual risk).
- goja is told not to follow source-map comments.

**Native recursion over run-time data** (`lib/runtime/jsharden.go`,
`lib/runtime/jsapi.go`, and the otto counterparts). A value the script nests at
run time (`var a=[]; for(...) a=[a]`) overflows the Go stack in native code that
recurses without a JavaScript call. `Array.prototype.join`, `toString`,
`toLocaleString`, `flat`, `flatMap` and `JSON.stringify` are replaced at hardening
time, before the freeze, with JavaScript implementations whose recursion over a
nested value goes through JavaScript calls, so goja's 1000-level call limit ends a
too-deep value with an uncatchable but safe error rather than a Go stack overflow.
They are output-identical to the natives on ordinary values, thrown errors
included (pinned by a parity test); string and number escaping is delegated to
the native `JSON.stringify` on primitives only. Each container's parts are joined
once natively, so the cost stays linear in the output. `join` is what `toString`,
a template literal, `+`, `String()` and the default `sort` comparator reach.
`flat` and `flatMap` always return a plain array and ignore `Symbol.species`.

Each nesting level costs several JavaScript frames, so the call limit ends a
value nested deeper than about 198 levels in `join`, `toString` and `String()`,
about 497 in `JSON.stringify` and about 997 in `toLocaleString` (measured). Real
data nests a few levels.

On otto the call limit already bounds the join family, so only `JSON.stringify`
is replaced. It orders members by key as otto's native does, also with a
replacer array, and costs about nine times the native.

The `moses` API sinks (`send`, `state.set`, and otto's command response) and
`console` take the value as an engine `Value`, not `interface{}`: the engines'
exporter recurses over the whole value while converting an `interface{}` argument,
before any guard could see it. Each sink instead converts the value in one bounded
pass that reads every property exactly once and never exports a composite
(`convertScriptValue`, `convertOttoValue`), so a getter or Proxy cannot answer a
pre-check shallow and the exporter deep. The pass bounds depth (1000 for a send,
the state depth for `state.set`), node count and total bytes of string values
and object keys (16 MiB), so a deep, wide, cyclic or gigabyte-string value, or
many objects sharing one huge key, is refused. A scalar is not walked, so the
common send of a number costs nothing.

A sink or `console` call made from a getter while another conversion runs is
refused: on goja `send` and `state.set` throw and `console` skips the call, on
otto the sink drops the value with a warning. Conversions therefore never nest;
the deepest one uses about 425 KB of Go stack at 1000 levels (measured), on top of
what the getter's own JavaScript uses under the call limit. `console` reads
nothing when its level is disabled, and formats with a writer that stops at
16 MiB per call across all arguments rather than rendering the whole value first.

Compared with the engine export these passes replaced, a function, Proxy, Map,
Set or typed array handed to `send` is dropped with a warning (it used to arrive
as a Go value no consumer could serialise, or unbounded), and a `String` object is
sent as its index-keyed characters instead of an empty object. On otto a number
array is now storable with `state.set`; the export made it an `[]int64`, which
`set` refused.

**Backtracking regexp timeout.** goja compiles a pattern RE2 cannot express
(lookaround, backreferences) to the regexp2 engine, whose match ignores the
script interrupt. `regexp2.DefaultMatchTimeout` is set process-wide to 250 ms
(`jsguard.RegexpMatchTimeout`) in a package `init`, before any script compiles,
and regexp2 copies it into every pattern - including the ones goja compiles
lazily - at compile time; goja reads a timed-out match as no match. 250 ms is
well under `JS_TIMEOUT` (2 s), so a pathological match overshoots the run by at
most that. otto translates every pattern to Go's RE2 (`regexp`), which is linear
and has no backtracking engine, so it needs no timeout.

Request bodies are limited to 16 MiB for documents (environment `PUT`/`POST`,
state `PATCH`, legacy world, room and device) and to 1 MiB for everything else
that carries JSON. A larger body is refused with `413` before it is decoded.

## One prepared VM per channel

A goja channel (`lib/runtime`) keeps one VM across its runs rather than making a
fresh one each time, because preparing a hardened VM costs ~7 ms and a run
costs microseconds. The VM is only ever used under its environment's mutex, so
no two goroutines touch one VM. It is discarded and rebuilt on a new generation
(a reload or a history run), on environment stop, and after any run that timed
out, overflowed the call limit, or whose cleanup did not complete.

A prepared VM keeps ~0.5 MB. At most 64 are kept per environment and 256 in the
process, least recently used first out, so kept VMs cost at most ~35 MB per
environment and ~140 MB in total. An environment the size of the Musterwerke
(44 script channels) stays fully cached. A channel past either cap gets a fresh
VM on its next run, which costs ~7 ms instead of microseconds.

Isolation between runs is restored rather than bought with a fresh VM: the
builtins are frozen once and moved onto a frozen prototype of the global
object, and after every run every global key the run added is deleted. The body runs inside a function
that receives `moses`, `httpGet` and `console` as parameters. For script
authors:

- **The body must be a program on its own.** A top-level `return` is a syntax
  error, as it always was, and a body cannot close the wrapper to run code at the
  top level.
- **A top-level `var` or `function` is local to the run**, not a property of the
  global object. `this.x` inside a function no longer finds a top-level `var x`;
  none of the known scripts does this.
- `var moses = moses` and `var console = console || {...}` keep working, and
  `arguments` at the top level is undefined as before (in a strict body it is
  the wrapper's).

An assignment to an undeclared name, or to `globalThis.x`, still works within a
run and is cleared before the next one. A script cannot extend or replace a
builtin (`Array.prototype.push = ...` throws or is ignored); a custom `toString`
or `constructor` on the script's own object still works.

**Shared state holds plain data only.** `state.set` in every scope accepts
null, booleans, numbers, strings, and arrays or plain objects of those, nested
at most 32 levels and holding at most 10,000 elements (`jsguard.MaxStateNodes`).
The element count includes every repeat, so a value that shares its subtrees
counts once per path and is refused before copying it could get exponential. A
function, a Date, a BigInt or a value of the `moses` API is refused with an
error, because another channel could otherwise run code inside the VM that
stored it. The known scripts store single scalars. `set` checks and copies in
one pass and stores only the copy, so a structure the script still holds cannot
change the stored value afterwards; `state.get` likewise returns a copy, never
the live map. The legacy runner drops a refused value with a warning.

A document's `context` and `initial_states` and a `PATCH` of the state meet the
same bounds, answered with `400` otherwise, so every stored value is one the
runtime can copy. A stored value past them anyway, cyclic, deeper than the copy
allows, or past the element budget (which only a corrupted state can hold), is
logged and dropped when read or snapshotted, instead of overflowing the stack
or exhausting memory.

## Crash brake

An unclosable fatal crash must not loop. `lib/crashbrake` records which
environment (goja) or world (otto) and goroutine was inside a script run, and the
next boot quarantines only the environment whose run's goroutine matches the crash
report, so it is not started back into the same crash.

- **Register.** A fixed file, memory-mapped `MAP_SHARED` (`SCRIPT_CRASH_DIR`),
  with 1024 slots. A slot is claimed per run by compare-and-swap at the start of
  the run and released when the run ends; concurrent in-flight runs are few.
  Closing the brake leaves the mapping in place, so a release that runs after
  shutdown still writes to valid memory. The goroutine id, the whole
  environment id and a (possibly truncated, informational) channel id are written
  as plain memory stores, no syscall; the kernel flushes the mapping even after
  the process dies. A run that finds no free slot is unprotected and logs that
  once. `CGO_ENABLED=0` keeps building; off linux, and with an empty
  `SCRIPT_CRASH_DIR`, the register is a no-op.
- **Ids.** The environment id is length-prefixed and stored whole, so an id with
  any byte in it decodes back to exactly itself; a torn entry is dropped, never
  read as another environment. The api refuses an environment id longer than a
  slot holds (`crashbrake.MaxEnvironmentIdBytes`, 256 bytes) or with a control
  character. An id stored before that bound is not covered; it still runs and is
  logged once as unprotected.
- **Crash report.** `debug.SetCrashOutput` points a fatal error's report at a
  file next to the register, emptied at every boot, so a report present at boot
  is from the previous process. Only a report whose first line starts with
  `panic:`, `fatal error:` or `runtime:` counts; a `SIGQUIT` dump, which also
  lists a running goroutine, does not. The boot check reads the crashing goroutine
  from the report's `goroutine N [running]:` line.
- **Boot decision.** Before any environment or world starts: quarantine an
  environment only when a crash report exists and its crashing goroutine matches a
  still-set slot. A report matching no slot, no report (SIGKILL, OOM kill,
  `os.Exit`, a Kafka `log.Fatal`), or a clean `SIGTERM` shutdown quarantines
  nothing.
- **Quarantine.** A quarantined environment is not started; its stored runtime
  state carries the reason and time (`RuntimeState.Quarantine`), exposed read-only
  as `quarantine` on `GET /environments/{id}`, and its stored history run is
  failed so a resume cannot restart the crashing script. The next reload (an edit)
  clears it. A quarantined legacy world is skipped for the run (`SkipWorldIds`).
  Each quarantine is logged once at ERROR with the id, channel and the first lines
  of the report. The decision holds the environment back this boot even when
  reading the environment or storing the quarantine fails; such a decision is
  kept, with the environment's version when it was read, and retried on the next
  boot, and dropped there if the environment was edited in between. A new crash
  of the same environment replaces a pending decision. A reload whose clear fails in the store leaves the environment
  quarantined.

## Residual risk

- **A memory bomb that is OOM-killed still loops.** A crash with no report
  (SIGKILL, OOM kill, node loss) quarantines nothing, since nothing ties the death
  to a script run. A script that exhausts memory rather than the stack is killed
  this way and comes back to the same crash, as before.
- **Native recursion no in-process guard reaches.** Four shapes recurse in goja's
  native code without a JavaScript call, so neither the call limit nor the script
  limits see them. Each is built in a loop and exhausts the 256 MB stack at
  roughly the size shown: a `yield*` delegation chain of 300,000 generators, a
  chain of 450,000 Proxies without traps followed by a property set, an
  `Object.create` prototype chain of 1,300,000 objects followed by a property
  set, and a `JSON.parse` reviver that injects a 900,000-level value mid-walk.
  Each is one fatal crash, which the brake turns into a quarantine, not a loop.
- **A native as an object's own `toString`.** One line crashes instantly on goja:
  `var o = {}; o.toString = String.prototype.trim; String(o)`. Any native that
  converts its receiver to a string does the same (`String.prototype.concat`,
  `Error.prototype.toString` with `e.name = e`), recursing without a JavaScript
  frame, so only a change to goja could stop it in process. otto ends it with a
  `RangeError`. It is one fatal crash, and the brake quarantines exactly that
  environment on the next boot (pinned by an end-to-end test).
- **A backtracking regexp still stalls its own environment.** The 250 ms timeout
  stops the match, but a script that fires many keeps the environment's lock and a
  CPU core for its whole 2 s run. The process and every other environment survive.
- **httpGet has no timeout.** A `httpGet` to an endpoint that never answers holds
  the environment's lock indefinitely; only that environment stalls.
- **Legacy-world quarantine is not durable.** It is re-derived from the register
  each boot, so it holds across the crash it is meant for, but a clean restart in
  between lets a quarantined world run again. Environments persist their quarantine.
- **What remains needs out-of-process isolation.** A single fatal crash still
  takes the process down once and drops the runs in flight in every other
  environment at that instant; only process isolation or a sandboxed engine
  removes that, a later stage.
