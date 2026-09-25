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

## What is guarded

Before parsing (`lib/jsguard`, applied by validation, by the runtime before each
compile and by the legacy runner before each run):

- **Size:** 32 KiB per script. This is the hard bound. goja's parser uses at
  most ~2.1 KB of stack per source byte (unclosed `(` or `[`; objects ~0.8 KB,
  every non-bracket chain such as `!!!…`, `x=>x=>…`, `a?b:…`, `a=a=…`, `**` or
  `if(1)if(1)…` at most ~0.16 KB). So 32 KiB costs at most ~68 MB, a ~15.9x
  margin under Go's 1 GB default max stack, whatever the depth scan makes of the
  script. The largest real script (Musterwerke) is 2948 bytes.
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
- Nested calls are limited to 1000 levels. goja ends the run with an
  uncatchable error, otto with an error. This bounds recursion through native
  functions (`forEach`, `sort`, getters, revivers, proxies) and, on otto, plain
  recursion.
- goja is told not to follow source-map comments.

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

## Residual risk

Deeply nested **data built at run time** can still overflow the stack in
native code that recurses without making a JavaScript call. Examples:
`String(a)` / `join` on `a = [a]` repeated about a million times (goja; otto's
join is bounded by the call limit), `JSON.stringify` of such a value (both
engines), and handing it to a Go function of the `moses` API or to `console`.
Closing this needs depth limits inside goja/otto or running scripts in a
separate process.

A catastrophically backtracking regexp on goja's regexp2 engine (a pattern
using lookaround or backreferences, such as `/^(a+)+(?!x)$/` against `a…ab`) is
not interrupted by the timeout. The run keeps the environment's lock and a CPU
core, so every channel of that environment stalls. The process survives.
