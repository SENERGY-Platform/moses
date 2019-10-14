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

package lib

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/robertkrimen/otto"
)

func startChangeRoutine(routine ChangeRoutine, callbacks map[string]interface{}, timeout time.Duration, mux sync.Locker) (ticker *time.Ticker, stop chan bool) {
	ticker = time.NewTicker(time.Duration(routine.Interval) * time.Second)
	stop = make(chan bool)
	go func() {
		for {
			select {
			case <-ticker.C:
				err := run(routine.Code, callbacks, timeout, mux)
				if err != nil {
					log.Println("ERROR: startChangeRoutine()", err)
				}
			case <-stop:
				return
			}
		}
	}()
	return
}

var halt = errors.New("stop")

func run(code string, moses interface{}, timeout time.Duration, mux sync.Locker) (err error) {
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
	vm.Interrupt = make(chan func(), 1) // The buffer prevents blocking

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
	if mux != nil {
		mux.Lock()
		defer mux.Unlock()
	}
	_, err = vm.Run(code) // Here be dragons (risky code)
	return
}
