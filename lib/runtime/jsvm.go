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

// Channel scripts run on goja, against a program compiled once per generation
// and a vm kept per channel whose globals are restored after every run
// (jsvms.go), so every run sees fresh globals as it did on otto. The timeout is
// armed only after the environment mutex has been taken:
// with one mutex per environment and many channels queueing on it, counting the
// wait for the lock against the script's time limit would turn a busy
// environment into a stream of spurious timeouts. lib/state keeps its own otto
// runner until the legacy package dies.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/scripthttp"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

var ErrScriptTimeout = errors.New("script exceeded the js timeout")

// errNoCompiledScript stands in when a binding reaches the script runner with
// neither a program nor a compile error, which would otherwise hand a nil
// program to the vm and take the process down.
var errNoCompiledScript = errors.New("the channel has no compiled script")

const maxCodeLogSize = 100

func trimCodeDefault(code string) string {
	return trimCode(code, maxCodeLogSize)
}

// trimCode keeps both ends: the start says what the script is, the end is where
// a syntax error usually sits.
func trimCode(code string, size int) string {
	if len(code) <= size {
		return code
	}
	return fmt.Sprintf("%v[...]%v", code[:size/2], code[len(code)-size/2:])
}

// compileScript compiles a channel script once, at generation build. The pre-scan
// makes a script able to overflow goja's parser fail this channel instead of
// killing the process; WithDisableSourceMaps stops a sourceMappingURL file read.
func compileScript(id string, code string) (*goja.Program, error) {
	if err := jsguard.ScriptTooComplex(code); err != nil {
		return nil, err
	}
	//the body has to be a program on its own: then it cannot close the wrapper
	//and leave code or a lexical declaration at the top level of the kept vm
	raw, err := goja.Parse(id, code, parser.WithDisableSourceMaps)
	if err != nil {
		return nil, err
	}
	shadowed := topLevelLexicalNames(raw)
	for _, wrapped := range wrapScript(code, shadowed) {
		var prg *ast.Program
		if prg, err = goja.Parse(id, wrapped, parser.WithDisableSourceMaps); err != nil {
			continue
		}
		var program *goja.Program
		if program, err = goja.CompileAST(prg, false); err == nil {
			return program, nil
		}
	}
	return nil, err
}

// topLevelLexicalNames returns the api names the body binds with let, const or
// class at the top level. Those shadow the global as they did on master, so the
// wrapper must not pass them as parameters, which a lexical redeclaration of a
// parameter would reject; var and function keep reading the parameter, as they
// read the global before.
func topLevelLexicalNames(program *ast.Program) map[string]bool {
	names := map[string]bool{}
	for _, statement := range program.Body {
		switch declaration := statement.(type) {
		case *ast.LexicalDeclaration:
			for _, binding := range declaration.List {
				boundNames(binding.Target, names)
			}
		case *ast.ClassDeclaration:
			if declaration.Class != nil && declaration.Class.Name != nil {
				names[declaration.Class.Name.Name.String()] = true
			}
		}
	}
	return names
}

// boundNames adds every name a binding target binds, through nested object and
// array patterns, defaults and rest elements.
func boundNames(target ast.Node, names map[string]bool) {
	switch node := target.(type) {
	case *ast.Identifier:
		names[node.Name.String()] = true
	case *ast.AssignExpression:
		boundNames(node.Left, names)
	case *ast.ArrayPattern:
		for _, element := range node.Elements {
			if element != nil {
				boundNames(element, names)
			}
		}
		if node.Rest != nil {
			boundNames(node.Rest, names)
		}
	case *ast.ObjectPattern:
		for _, property := range node.Properties {
			switch p := property.(type) {
			case *ast.PropertyShort:
				names[p.Name.Name.String()] = true
			case *ast.PropertyKeyed:
				boundNames(p.Value, names)
			}
		}
		if node.Rest != nil {
			boundNames(node.Rest, names)
		}
	}
}

// wrapScript makes the body a function called with the global this, so a kept vm
// gives every run fresh declarations. The api names the body does not lexically
// declare come in as parameters, so "var moses = moses" still finds them; an
// undefined arguments parameter keeps the wrapper's own from showing, and a
// strict body that refuses that name falls to the second form. The body starts
// on the first line, keeping line numbers; a hashbang becomes a comment.
func wrapScript(code string, shadowed map[string]bool) []string {
	if strings.HasPrefix(code, "#!") {
		code = "//" + code[2:]
	}
	var params []string
	for _, name := range []string{"moses", "httpGet", "console"} {
		if !shadowed[name] {
			params = append(params, name)
		}
	}
	call := "\n}).call(this"
	if len(params) > 0 {
		call += ", " + strings.Join(params, ", ")
	}
	call += ");"
	withArguments := strings.Join(append(append([]string{}, params...), "arguments"), ", ")
	return []string{
		"(function (" + withArguments + ") {" + code + call,
		"(function (" + strings.Join(params, ", ") + ") {" + code + call,
	}
}

// runScript executes program once on a vm of its own, which is what a test
// wants; the runtime keeps a channel's vm through runScriptIn.
func runScript(program *goja.Program, moses interface{}, timeout time.Duration, mux sync.Locker) error {
	return runScriptIn(nil, nil, program, moses, timeout, mux)
}

// runScriptIn executes program with moses bound to the javascript global "moses",
// on the vm vms keeps for it, or on a fresh one when vms is nil.
//
// mux serialises the runs of one environment. It is held for the duration of
// the script, which is what makes the state maps the script reads and writes
// safe to touch without any locking of their own - and what gives a script the
// same "nothing else changes while I run" guarantee the legacy world mutex gave.
// vms is only touched under it, so no two goroutines ever share a vm.
//
// The timeout is armed after the vm is ready, so neither the lock wait nor
// preparing a vm counts against it. A vm is kept only after a run that ended
// normally and whose cleanup succeeded; any other run discards it.
//
// Two semantic differences to otto are accepted here: goja does not hoist a
// function declared inside a block, so Annex B block-level function
// declarations are only visible inside that block. And an integral number
// arrives in Go as a float64, so a value above 2^53 is no longer exact.
func runScriptIn(vms *scriptVMs, gen *generation, program *goja.Program, moses interface{}, timeout time.Duration, mux sync.Locker) error {
	return runScriptInBraked(vms, gen, program, moses, timeout, mux, nil, "", "", nil, nil)
}

// runScriptInBraked is runScriptIn plus the crash-brake entry marked in flight for
// the duration of the run, inside the same lock, so a fatal crash names this
// environment and goroutine on the next boot. scriptHTTP serves the script's
// httpGet; nil refuses every request.
func runScriptInBraked(vms *scriptVMs, gen *generation, program *goja.Program, moses interface{}, timeout time.Duration, mux sync.Locker, brake *crashbrake.Brake, environmentId string, channelId string, guard *jsguard.SinkGuard, scriptHTTP *scripthttp.Client) error {
	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
	}
	//inside the lock, so the runs of one environment never overlap: a fatal crash
	//from here until the release names this environment and goroutine on the next boot
	if brake != nil {
		defer brake.Enter(environmentId, channelId)()
	}
	var prepared *scriptVM
	var err error
	if vms != nil {
		prepared, err = vms.take(gen, program)
	} else {
		prepared, err = newScriptVM()
	}
	if err != nil {
		util.Logger.Warn("unable to prepare the javascript vm", attributes.ErrorKey, err)
		return err
	}
	reusable := false
	defer func() {
		if vms != nil && !reusable {
			vms.discard(program)
		}
	}()

	vm := prepared.vm
	vm.ClearInterrupt()
	if err := vm.Set("moses", moses); err != nil {
		return err
	}
	//httpGet's requests end with the run's timer, which is armed right below. A
	//request cut off by it also marks the run, since goja checks the interrupt only
	//between instructions and a promise job or a getter calls the sinks natively
	deadline := time.Now().Add(timeout)
	expired := false
	if guard != nil {
		guard.StartRun()
	}
	expire := func() {
		expired = true
		if guard != nil {
			guard.Expire()
		}
		vm.Interrupt(ErrScriptTimeout)
	}
	if err := vm.Set("httpGet", httpGetUntil(scriptHTTP, deadline, expire)); err != nil {
		util.Logger.Warn("unable to set up httpGet in javascript vm", attributes.ErrorKey, err)
		return err
	}
	if err := vm.Set("console", scriptConsole(guard)); err != nil {
		util.Logger.Warn("unable to set up console in javascript vm", attributes.ErrorKey, err)
		return err
	}

	fired := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() {
		if delay := timeoutCallbackDelay.Load(); delay > 0 {
			time.Sleep(time.Duration(delay))
		}
		vm.Interrupt(ErrScriptTimeout)
		close(fired)
	})
	defer timer.Stop()              // covers a panic out of a native binding
	_, err = vm.RunProgram(program) // Here be dragons (risky code)
	late := !timer.Stop()
	if late {
		//the callback has started: wait until its interrupt has landed, so it can
		//never overlap a later run's ClearInterrupt, and do not keep this vm
		<-fired
	}

	var interrupted *goja.InterruptedError
	var overflow *goja.StackOverflowError
	switch {
	case errors.As(err, &interrupted) || expired:
		return ErrScriptTimeout
	case late || errors.As(err, &overflow):
	default:
		reusable = prepared.restore()
	}
	return err
}

// timeoutCallbackDelay holds back the timeout callback before it interrupts;
// only a test sets it, to force the callback past the end of a run.
var timeoutCallbackDelay atomic.Int64

// scriptConsole is the console object otto shipped and goja does not. Its methods
// are native goja functions so goja does not export (and recurse over) an
// argument; each argument is read in one bounded pass instead, so console.log of
// a deeply nested value cannot overflow the stack.
func scriptConsole(guard *jsguard.SinkGuard) map[string]interface{} {
	//nothing is formatted when the level is disabled, and a call re-entered from a
	//getter while its own arguments are being converted is skipped, so a disabled
	//log costs nothing and one conversion never nests another.
	emit := func(level slog.Level, logfn func(string, ...interface{}), call goja.FunctionCall) {
		if !util.Logger.Enabled(context.Background(), level) {
			return
		}
		if guard != nil {
			if !guard.Enter() {
				return
			}
			defer guard.Leave()
		}
		logfn("script console", "arguments", joinScriptArgs(call.Arguments))
	}
	debug := func(call goja.FunctionCall) goja.Value {
		emit(slog.LevelDebug, util.Logger.Debug, call)
		return goja.Undefined()
	}
	warn := func(call goja.FunctionCall) goja.Value {
		emit(slog.LevelWarn, util.Logger.Warn, call)
		return goja.Undefined()
	}
	return map[string]interface{}{
		"log":   debug,
		"info":  debug,
		"debug": debug,
		"warn":  warn,
		"error": warn,
	}
}

// maxConsoleBytes bounds the total rendered size of one console call across all
// its arguments, so many huge arguments cannot make the formatter allocate
// gigabytes. Conversion stops once the budget is spent.
const maxConsoleBytes = 16 << 20

// maxConsoleBigIntBits bounds the BigInt console prints in full, about 20,000
// decimal digits.
const maxConsoleBigIntBits = 1 << 16

// joinScriptArgs renders what a script passed to console as one log attribute,
// each argument read in one bounded pass, and the whole call bounded to
// maxConsoleBytes so a deep or huge value is summarised rather than exported.
func joinScriptArgs(args []goja.Value) string {
	var b strings.Builder
	for i, arg := range args {
		if b.Len() >= maxConsoleBytes {
			b.WriteString(" [truncated]")
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		if converted, err := convertScriptValue(arg, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes); err == nil {
			writeBounded(&b, converted, maxConsoleBytes)
		} else {
			writeBounded(&b, "[unloggable value: "+err.Error()+"]", maxConsoleBytes)
		}
	}
	return b.String()
}

// writeBounded writes value as fmt.Sprint would, but stops once b holds limit
// bytes (overshooting by at most a separator), so formatting never allocates
// much more than the limit.
func writeBounded(b *strings.Builder, value interface{}, limit int) {
	if b.Len() >= limit {
		return
	}
	switch v := value.(type) {
	case string:
		if room := limit - b.Len(); len(v) > room {
			v = v[:room]
		}
		b.WriteString(v)
	case []interface{}:
		b.WriteByte('[')
		for i, element := range v {
			if b.Len() >= limit {
				return
			}
			if i > 0 {
				b.WriteByte(' ')
			}
			writeBounded(b, element, limit)
		}
		if b.Len() < limit {
			b.WriteByte(']')
		}
	case map[string]interface{}:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteString("map[")
		for i, key := range keys {
			if b.Len() >= limit {
				return
			}
			if i > 0 {
				b.WriteByte(' ')
			}
			writeBounded(b, key, limit)
			b.WriteByte(':')
			writeBounded(b, v[key], limit)
		}
		if b.Len() < limit {
			b.WriteByte(']')
		}
	case *big.Int:
		//a huge BigInt would be expensive to print in decimal
		if v.BitLen() > maxConsoleBigIntBits {
			writeBounded(b, fmt.Sprintf("[BigInt of %d bits]", v.BitLen()), limit)
			return
		}
		writeBounded(b, v.String(), limit)
	default:
		//a scalar or a Date, small whatever the script built
		writeBounded(b, fmt.Sprint(v), limit)
	}
}

// httpGetUntil is the script's httpGet for a run whose requests end at deadline.
// A request cut off by it interrupts the run before the script's next
// instruction, so a statement never stores the empty answer of a timed-out run.
func httpGetUntil(client *scripthttp.Client, deadline time.Time, expire func()) func(endpoint string) string {
	return func(endpoint string) string {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		body, err := client.Get(ctx, endpoint)
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				expire()
			}
			return ""
		}
		return body
	}
}
