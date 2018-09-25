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
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"strings"
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

func TestJsvmApi(t *testing.T) {
	w := World{
		Id:   "world_2",
		Name: "World2",
		Rooms: map[string]*Room{
			"room_1": &Room{
				Id:   "room_1",
				Name: "Room1",
				States: map[string]interface{}{
					"answer":  float64(0),
					"counter": float64(0),
				},
				Devices: map[string]*Device{
					"device_2": &Device{
						Id:   "device_2",
						Name: "Device2",
						States: map[string]interface{}{
							"answer": float64(42),
						},
						ChangeRoutines: []ChangeRoutine{
							{
								Interval: 500 * time.Millisecond,
								Code: `
									var deviceAnswer = moses.device.state.get("answer");
									moses.room.state.set("answer", deviceAnswer);
									moses.world.state.set("answer", deviceAnswer);
									var roomCounter = moses.room.state.get("counter");
									roomCounter = roomCounter + 1;
									moses.room.state.set("counter", roomCounter);
								`,
							},
						},
					},
				},
			},
		},
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	req, err := http.NewRequest("PUT", testserver.URL+"/world", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}

	time.Sleep(2 * time.Second)

	resp, err = http.Get(testserver.URL + "/world/world_2")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	w2 := World{}
	err = json.NewDecoder(resp.Body).Decode(&w2)
	if err != nil {
		t.Fatal(err)
	}

	w.States = map[string]interface{}{}
	w.States["answer"] = float64(42)
	w.Rooms["room_1"].States["answer"] = float64(42)
	w.Rooms["room_1"].States["counter"] = float64(3)
	if !reflect.DeepEqual(w, w2) {
		t.Fatal("unexpected result", w, w2)
	}
}
