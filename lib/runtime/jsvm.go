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
// and a fresh vm per run, so every run sees fresh globals exactly as it did on
// otto. The timeout is armed only after the environment mutex has been taken:
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
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/dop251/goja"
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

// compileScript compiles a channel script once, at generation build. The id
// only names the script in a syntax error.
func compileScript(id string, code string) (*goja.Program, error) {
	return goja.Compile(id, code, false)
}

// runScript executes program with moses bound to the javascript global "moses".
//
// mux serialises the runs of one environment. It is held for the duration of
// the script, which is what makes the state maps the script reads and writes
// safe to touch without any locking of their own - and what gives a script the
// same "nothing else changes while I run" guarantee the legacy world mutex gave.
//
// The timeout is armed after the mutex has been acquired, so the wait for the
// lock does not count against the script's time limit. The timer is stopped on
// every path out, including a panic, because an armed timer keeps the vm and the
// whole api closure graph reachable until the timeout elapses; a timer that
// fires anyway interrupts a vm nobody uses again, since every run gets its own.
//
// Two semantic differences to otto are accepted here: goja does not hoist a
// function declared inside a block, so Annex B block-level function
// declarations are only visible inside that block. And an integral number
// arrives in Go as a float64, so a value above 2^53 is no longer exact.
func runScript(program *goja.Program, moses interface{}, timeout time.Duration, mux sync.Locker) error {
	vm := goja.New()
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

	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
	}

	timer := time.AfterFunc(timeout, func() { vm.Interrupt(ErrScriptTimeout) })
	defer timer.Stop()
	_, err := vm.RunProgram(program) // Here be dragons (risky code)

	//goja reports the halt as an error rather than a panic; the interrupt value
	//travels inside it, but callers match on ErrScriptTimeout alone
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return ErrScriptTimeout
	}
	return err
}

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
