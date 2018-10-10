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
	"fmt"
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
						Id:          "device_2",
						Name:        "Device2",
						ExternalRef: "device_2",
						States: map[string]interface{}{
							"answer": float64(42),
						},
						ChangeRoutines: map[string]ChangeRoutine{
							"5": {
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
	req, err := http.NewRequest("PUT", mockserver.URL+"/dev/world", strings.NewReader(string(b)))
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

	resp, err = http.Get(mockserver.URL + "/dev/world/world_2")
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

	var compa interface{}
	var compb interface{}

	abuffer, _ := json.Marshal(w)
	json.Unmarshal(abuffer, &compa)

	bbuffer, _ := json.Marshal(w2)
	json.Unmarshal(bbuffer, &compb)

	if !reflect.DeepEqual(compa, compb) {
		t.Fatal("unexpected result", compa, compb)
	}
}

func TestJsvmApi2(t *testing.T) {
	w := World{
		Id:   "world_3",
		Name: "World2",
		States: map[string]interface{}{
			"temp": float64(10),
		},
		Rooms: map[string]*Room{
			"room_1": &Room{
				Id:   "room_1",
				Name: "Room1",
				States: map[string]interface{}{
					"temp": float64(20),
				},
			},
		},
		ChangeRoutines: map[string]ChangeRoutine{
			"4": {
				Interval: 500 * time.Millisecond,
				Code: `
						//Example for World-Change-Routine
						//room temperature is influenced by the world
						var temperature = moses.world.state.get("temp");
						var room_temperature = moses.world.getRoom("room_1").state.get("temp");
						if(temperature > room_temperature){
						    room_temperature = room_temperature + 1;
						}else if(temperature < room_temperature){
						    room_temperature = room_temperature - 1;
						}
						moses.world.getRoom("room_1").state.set("temp", room_temperature);
				`,
			},
		},
	}

	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	req, err := http.NewRequest("PUT", mockserver.URL+"/dev/world", strings.NewReader(string(b)))
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

	resp, err = http.Get(mockserver.URL + "/dev/world/world_3")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	w2 := World{}
	err = json.NewDecoder(resp.Body).Decode(&w2)
	if err != nil {
		t.Fatal(err)
	}

	newRoomTemp := w2.Rooms["room_1"].States["temp"]
	oldRoomTemp := w.Rooms["room_1"].States["temp"]
	if newRoomTemp.(float64) != oldRoomTemp.(float64)-3 && newRoomTemp.(float64) != oldRoomTemp.(float64)-4 {
		t.Fatal("unexpected result", newRoomTemp.(float64), oldRoomTemp.(float64)-3)
	}
}

func ExampleJsvmService() {
	w := World{
		Id:   "world_4",
		Name: "World2",
		States: map[string]interface{}{
			"temp": float64(10),
		},
		Rooms: map[string]*Room{
			"room_1": &Room{
				Id:   "room_1",
				Name: "Room1",
				States: map[string]interface{}{
					"temp":   float64(30),
					"answer": float64(42),
				},
				Devices: map[string]*Device{
					"device_s1": {
						Id:          "device_s1",
						ExternalRef: "device_s1",
						Services: map[string]Service{
							"sensor_s1": {
								Id:             "sensor_s1",
								ExternalRef:    "sensor_s1",
								Name:           "SenseTemp",
								SensorInterval: 230 * time.Millisecond,
								Code: `
									var temp = moses.room.state.get("temp");
									moses.service.send(temp);
								`,
							},
							"actuator_a1": {
								Id:          "actuator_a1",
								ExternalRef: "actuator_a1",
								Name:        "IncreaseTempBy",
								Code: `
									var answer = moses.room.state.get("answer");
									var temp = moses.room.state.get("temp");
									temp = temp + moses.service.input.temp;
									moses.room.state.set("temp", temp);
									moses.service.send({"answer":answer, "temp":temp});
								`,
							},
						},
					},
				},
			},
		},
		ChangeRoutines: map[string]ChangeRoutine{
			"3": {
				Interval: 200 * time.Millisecond,
				Code: `
						//Example for World-Change-Routine
						//room temperature is influenced by the world
						var temperature = moses.world.state.get("temp");
						var room_temperature = moses.world.getRoom("room_1").state.get("temp");
						if(temperature > room_temperature){
						    room_temperature = room_temperature + 1;
						}else if(temperature < room_temperature){
						    room_temperature = room_temperature - 1;
						}
						moses.world.getRoom("room_1").state.set("temp", room_temperature);
				`,
			},
		},
	}

	b, err := json.Marshal(w)
	if err != nil {
		fmt.Println(err)
	}
	client := &http.Client{}
	req, err := http.NewRequest("PUT", mockserver.URL+"/dev/world", strings.NewReader(string(b)))
	if err != nil {
		fmt.Println(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		fmt.Println(resp.Status, string(body))
	}

	time.Sleep(1 * time.Second)

	//send command to actuator service
	test_receiver("device_s1", "actuator_a1", map[string]interface{}{"temp": 8}, func(respMsg interface{}) {
		resp, ok := respMsg.(map[string]interface{})
		if ok {
			fmt.Println("response", len(resp), resp["answer"], resp["temp"])
		}
	})

	time.Sleep(1 * time.Second)

	//add room to test start and stop of world
	r2 := Room{
		Id:   "room_2",
		Name: "room_2",
	}
	b, err = json.Marshal(r2)
	if err != nil {
		fmt.Println(err)
	}
	client = &http.Client{}
	req, err = http.NewRequest("PUT", mockserver.URL+"/dev/world/world_4/room", strings.NewReader(string(b)))
	if err != nil {
		fmt.Println(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		fmt.Println(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		fmt.Println(resp.Status, string(body))
	}

	time.Sleep(2 * time.Second)

	fmt.Println(test_send_values)

	//output:
	//response 2 42 33
	//[{device_s1 sensor_s1 29} {device_s1 sensor_s1 28} {device_s1 sensor_s1 27} {device_s1 sensor_s1 26} {device_s1 sensor_s1 33} {device_s1 sensor_s1 32} {device_s1 sensor_s1 30} {device_s1 sensor_s1 29} {device_s1 sensor_s1 27} {device_s1 sensor_s1 26} {device_s1 sensor_s1 25} {device_s1 sensor_s1 24} {device_s1 sensor_s1 23} {device_s1 sensor_s1 22} {device_s1 sensor_s1 20} {device_s1 sensor_s1 19}]
}

func _ExampleJsvmService() {
	w := World{
		Id:   "world_4",
		Name: "World2",
		States: map[string]interface{}{
			"temp": float64(10),
		},
		Rooms: map[string]*Room{
			"room_1": &Room{
				Id:   "room_1",
				Name: "Room1",
				States: map[string]interface{}{
					"temp":   float64(30),
					"answer": float64(42),
				},
				Devices: map[string]*Device{
					"device_s1": {
						Id:     "device_s1",
						States: map[string]interface{}{},
						ChangeRoutines: map[string]ChangeRoutine{"1": {
							Interval: 1 * time.Hour,
							Code: `
								var rtemp = moses.room.state.get("temp");	
								var soll_temp = moses.device.state.get("soll_temp");	
								if(rtemp > soll_temp){
						    		rtemp = rtemp + 0.5;
								}else if(rtemp < soll_temp){
						    		rtemp = rtemp - 0.5;
								}
								moses.room.state.set("temp", rtemp);
							`,
						}},
						Services: map[string]Service{
							"sensor_s1": {
								Id:             "sensor_s1",
								Name:           "SenseTemp",
								SensorInterval: 230 * time.Millisecond,
								Code: `
									var temp = moses.room.state.get("temp");
									moses.service.send(temp);
								`,
							},
							"actuator_a1": {
								Id:   "actuator_a1",
								Name: "IncreaseTempBy",
								Code: `
									temp = temp + moses.service.input.temp;
									moses.device.state.set("soll_temp", temp);
									moses.service.send(null);
								`,
							},
						},
					},
				},
			},
		},
		ChangeRoutines: map[string]ChangeRoutine{
			"2": {
				Interval: 200 * time.Millisecond,
				Code: `
						//Example for World-Change-Routine
						//room temperature is influenced by the world
						var temperature = moses.world.state.get("temp");
						var room_temperature = moses.world.getRoom("room_1").state.get("temp");
						if(temperature > room_temperature){
						    room_temperature = room_temperature + 1;
						}else if(temperature < room_temperature){
						    room_temperature = room_temperature - 1;
						}
						moses.world.getRoom("room_1").state.set("temp", room_temperature);
				`,
			},
		},
	}

	b, err := json.Marshal(w)
	if err != nil {
		fmt.Println(err)
	}
	client := &http.Client{}
	req, err := http.NewRequest("PUT", mockserver.URL+"/dev/world", strings.NewReader(string(b)))
	if err != nil {
		fmt.Println(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		fmt.Println(resp.Status, string(body))
	}

	time.Sleep(1 * time.Second)

	//send command to actuator service
	test_receiver("device_s1", "actuator_a1", map[string]interface{}{"temp": 8}, func(respMsg interface{}) {
		resp, ok := respMsg.(map[string]interface{})
		if ok {
			fmt.Println("response", len(resp), resp["answer"], resp["temp"])
		}
	})

	time.Sleep(1 * time.Second)

	//add room to test start and stop of world
	r2 := Room{
		Id:   "room_2",
		Name: "room_2",
	}
	b, err = json.Marshal(r2)
	if err != nil {
		fmt.Println(err)
	}
	client = &http.Client{}
	req, err = http.NewRequest("PUT", mockserver.URL+"/dev/world/world_4/room", strings.NewReader(string(b)))
	if err != nil {
		fmt.Println(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		fmt.Println(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		fmt.Println(resp.Status, string(body))
	}

	time.Sleep(2 * time.Second)

	fmt.Println(test_send_values)

	//output:
	//response 2 42 33
	//[{device_s1 sensor_s1 29} {device_s1 sensor_s1 28} {device_s1 sensor_s1 27} {device_s1 sensor_s1 26} {device_s1 sensor_s1 33} {device_s1 sensor_s1 32} {device_s1 sensor_s1 30} {device_s1 sensor_s1 29} {device_s1 sensor_s1 27} {device_s1 sensor_s1 26} {device_s1 sensor_s1 25} {device_s1 sensor_s1 24} {device_s1 sensor_s1 23} {device_s1 sensor_s1 22} {device_s1 sensor_s1 20} {device_s1 sensor_s1 19}]
}
