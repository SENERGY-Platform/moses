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
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
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
	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
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
	if err := vm.Set("httpGet", httpGet); err != nil {
		util.Logger.Warn("unable to set up httpGet in javascript vm", attributes.ErrorKey, err)
		return err
	}
	if err := vm.Set("console", scriptConsole()); err != nil {
		util.Logger.Warn("unable to set up console in javascript vm", attributes.ErrorKey, err)
		return err
	}

	fired := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
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
	case errors.As(err, &interrupted):
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

// scriptConsole is the console object otto shipped and goja does not. A legacy
// script was migrated verbatim and may call console.log, which without this
// binding is a ReferenceError that aborts the run before its first send.
func scriptConsole() map[string]interface{} {
	debug := func(args ...interface{}) {
		util.Logger.Debug("script console", "arguments", joinScriptArgs(args))
	}
	warn := func(args ...interface{}) {
		util.Logger.Warn("script console", "arguments", joinScriptArgs(args))
	}
	return map[string]interface{}{
		"log":   debug,
		"info":  debug,
		"debug": debug,
		"warn":  warn,
		"error": warn,
	}
}

// joinScriptArgs renders what a script passed to console as one attribute, so
// a call with several arguments stays one log line.
func joinScriptArgs(args []interface{}) string {
	return strings.TrimSuffix(fmt.Sprintln(args...), "\n")
}

// httpGet is part of the script surface a migrated script may already use.
func httpGet(endpoint string) string {
	resp, err := http.Get(endpoint)
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
