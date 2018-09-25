/*
 * Copyright 2018 SENERGY Team
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

package main

import (
	"log"
	"testing"
	"time"
)

type JsvmTestMoses struct {
	State   map[string]interface{}
	Send    func(field string, val interface{})
	Receive func(field string) interface{}
}

func TestJsvmRun(t *testing.T) {
	script := `
	var foo = 2*21;
	moses.Send("foo", foo);
	var bar = moses.Receive("bar");
	console.log(bar);
	moses.Send("bar", "foo "+bar);
`
	testmoses := JsvmTestMoses{State: map[string]interface{}{"bar": "bar"}}
	testmoses.Send = func(field string, val interface{}) {
		log.Println("send: ", val)
		testmoses.State[field] = val
	}
	testmoses.Receive = func(field string) interface{} {
		log.Println("receive", field)
		return testmoses.State[field]
	}

	err := run(script, testmoses, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}

	if testmoses.State["bar"].(string) != "foo bar" || testmoses.State["foo"].(float64) != 42 {
		t.Fatal("unexpected testmoses state", testmoses)
	}
}

func TestJsvmTimeout(t *testing.T) {
	script := `
	while(true){}
`
	testmoses := JsvmTestMoses{State: map[string]interface{}{}}
	done := false
	go func() {
		time.Sleep(1 * time.Second)
		if !done {
			t.Fatal("slept to long")
		}
	}()
	err := run(script, testmoses, 100*time.Millisecond, nil)
	if err == nil {
		t.Fatal("missing error; should have thrown 'Some code took to long' after 100 ms")
	}
	done = true
}
