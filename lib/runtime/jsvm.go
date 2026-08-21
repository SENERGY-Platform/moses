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

// This file is a deliberate copy of the otto runner in lib/state/jsvm.go, not a
// shared helper. The new runtime must not import the legacy package: the legacy
// one is going away, and an import would keep it alive and let a change to the
// old runner reach the new one. The duplication is temporary and dies with
// lib/state.
//
// Two things are deliberately NOT copied verbatim, both marked below: when the
// interrupt timer is armed, and how the timer goroutine ends. Both are bugs in
// the legacy version that only became load bearing here, because a single
// environment mutex now serialises many channels.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/robertkrimen/otto"
)

// halt is the panic value the interrupt handler raises. otto has no other way
// to stop a running script.
var halt = errors.New("stop")

// ErrScriptTimeout is returned when a script was interrupted because it ran
// longer than the configured js timeout.
var ErrScriptTimeout = errors.New("script exceeded the js timeout")

const maxCodeLogSize = 100

func trimCodeDefault(code string) string {
	return trimCode(code, maxCodeLogSize)
}

// trimCode shortens a script for a log line, keeping both ends: the beginning
// says what the script is, the end is where a syntax error usually sits.
func trimCode(code string, size int) string {
	if len(code) <= size {
		return code
	}
	return fmt.Sprintf("%v[...]%v", code[:size/2], code[len(code)-size/2:])
}

// run executes code with moses bound to the javascript global "moses".
//
// mux serialises the runs of one environment. It is held for the duration of
// the script, which is what makes the state maps the script reads and writes
// safe to touch without any locking of their own - and what gives a script the
// same "nothing else changes while I run" guarantee the legacy world mutex gave.
//
// DEVIATION from lib/state/jsvm.go, on purpose: the timeout is armed AFTER the
// mutex has been acquired. The legacy version armed it before, so the wait for
// the mutex counted against the script's time limit, and, because otto's
// interrupt channel is buffered, a script that waited too long for the lock was
// killed in its first statement. With one mutex per environment and many
// channels queueing on it that would turn a busy environment into a stream of
// spurious timeouts.
func run(code string, moses interface{}, timeout time.Duration, mux sync.Locker) (err error) {
	defer func() {
		if caught := recover(); caught != nil {
			if caught == halt {
				err = ErrScriptTimeout
				return
			}
			panic(caught) // Something else happened, repanic!
		}
	}()

	vm := otto.New()
	vm.Interrupt = make(chan func(), 1) // The buffer prevents blocking

	err = vm.Set("moses", moses)
	if err != nil {
		return err
	}

	err = vm.Set("httpGet", httpGet)
	if err != nil {
		util.Logger.Warn("unable to set up httpGet in javascript vm", attributes.ErrorKey, err)
		return err
	}

	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
	}

	// DEVIATION from lib/state/jsvm.go, on purpose: the watchdog ends with the
	// run instead of sleeping out its full timeout. The legacy version leaked a
	// sleeping goroutine per execution, which a one second channel with a two
	// second timeout keeps several of alive at all times.
	done := make(chan struct{})
	defer close(done)
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			//buffered, so this never blocks even if the vm is already finished
			vm.Interrupt <- func() {
				panic(halt)
			}
		case <-done:
		}
	}()

	_, err = vm.Run(code) // Here be dragons (risky code)
	return err
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
