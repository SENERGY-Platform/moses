/*
 * Copyright 2019 InfAI (CC SES)
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

package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/crashbrake"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/scripthttp"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/robertkrimen/otto"
)

func startChangeRoutine(routine ChangeRoutine, callbacks map[string]interface{}, timeout time.Duration, mux sync.Locker, brake *crashbrake.Brake, worldId string, locationInfoForErrorLogging string, scriptHTTP *scripthttp.Client) (ticker *time.Ticker, stop chan bool) {
	ticker = time.NewTicker(time.Duration(routine.Interval) * time.Second)
	stop = make(chan bool)
	go func() {
		for {
			select {
			case <-ticker.C:
				err := run(routine.Code, callbacks, timeout, mux, brake, worldId, routine.Id, scriptHTTP)
				if err != nil {
					util.Logger.Warn("change routine failed", attributes.ErrorKey, err, "location", locationInfoForErrorLogging, "code", trimCodeDefault(routine.Code))
				}
			case <-stop:
				return
			}
		}
	}()
	return
}

const maxCodeLogSize = 100

func trimCodeDefault(code string) string {
	return trimCode(code, maxCodeLogSize)
}

func trimCode(code string, size int) string {
	if len(code) <= size {
		return code
	}
	return fmt.Sprintf("%v[...]%v", code[:size/2], code[len(code)-size/2:])
}

var halt = errors.New("stop")

// run executes a legacy script. The complexity check runs before otto parses
// it, since otto's parser overflows the Go stack on deep nesting, a fatal crash;
// this covers stored routines at load, on every tick, and service commands.
// scriptHTTP serves the script's httpGet; nil refuses every request.
func run(code string, moses interface{}, timeout time.Duration, mux sync.Locker, brake *crashbrake.Brake, worldId string, channelId string, scriptHTTP *scripthttp.Client) (err error) {
	if err := jsguard.ScriptTooComplex(code); err != nil {
		return err
	}
	defer func() {
		if caught := recover(); caught != nil {
			if caught == halt {
				err = errors.New("Some code took to long")
				return
			}
			panic(caught) // Something else happened, repanic!
		}
	}()

	vm := otto.New()
	if err = hardenVM(vm); err != nil {
		return err
	}
	vm.Interrupt = make(chan func(), 1) // The buffer prevents blocking

	//httpGet shares the interrupt's budget, which counts from here, including the wait for the world
	deadline := time.Now().Add(timeout)
	go func() {
		time.Sleep(timeout) // Stop after two seconds
		vm.Interrupt <- func() {
			panic(halt)
		}
	}()
	err = vm.Set("moses", moses)
	if err != nil {
		return
	}

	err = vm.Set("httpGet", httpGetUntil(scriptHTTP, deadline))
	if err != nil {
		util.Logger.Warn("unable to set up httpGet in javascript vm", attributes.ErrorKey, err)
		return
	}

	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
	}
	//inside the world lock, so runs of one world never overlap: a fatal crash from
	//here until the release names this world and goroutine on the next boot
	if brake != nil {
		defer brake.Enter(worldId, channelId)()
	}
	_, err = vm.Run(code) // Here be dragons (risky code)
	return
}

// httpGetUntil is the script's httpGet for a run whose requests end at deadline. A request cut off by the deadline
// ends the run as the interrupt does, so the rest of the statement never sees an empty answer it would store.
func httpGetUntil(client *scripthttp.Client, deadline time.Time) func(endpoint string) string {
	return func(endpoint string) string {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		body, err := client.Get(ctx, endpoint)
		if err != nil {
			haltOnDeadline(ctx)
			return ""
		}
		return body
	}
}

// haltOnDeadline ends the run like the interrupt when the request failed because the run's time is up.
func haltOnDeadline(ctx context.Context) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		panic(halt)
	}
}
