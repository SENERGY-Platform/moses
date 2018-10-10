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
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestDevApi(t *testing.T) {
	w := World{Id: "world_1", Name: "World1", Rooms: map[string]*Room{"room_1": &Room{Id: "room_1", Name: "Room1", Devices: map[string]*Device{"device_1": &Device{Id: "device_1", Name: "Device1"}}}}}
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

	resp, err = http.Get(mockserver.URL + "/dev/worlds")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	worlds := map[string]World{}
	err = json.NewDecoder(resp.Body).Decode(&worlds)
	if err != nil {
		t.Fatal(err)
	}
	if len(worlds) != 1 {
		t.Fatal(worlds)
	}

	resp, err = http.Get(mockserver.URL + "/dev/world/world_1")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	w2 := World{}
	err = json.NewDecoder(resp.Body).Decode(&w2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, w2) {
		t.Fatal("unequal ", w, w2)
	}

	// second device

	d2 := Device{Id: "device_2", Name: "Device2"}
	b, err = json.Marshal(d2)
	if err != nil {
		t.Fatal(err)
	}
	client = &http.Client{}
	req, err = http.NewRequest("PUT", mockserver.URL+"/dev/world/world_1/room/room_1/device", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}

	w.Rooms["room_1"].Devices["device_2"] = &d2

	resp, err = http.Get(mockserver.URL + "/dev/world/world_1")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	w3 := World{}
	err = json.NewDecoder(resp.Body).Decode(&w3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, w3) {
		t.Fatal("unequal ", w, w3)
	}

	resp, err = http.Get(mockserver.URL + "/dev/world/world_1/room/room_1/device/device_2")
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		t.Fatal(resp.Status, string(body))
	}
	d := Device{}
	err = json.NewDecoder(resp.Body).Decode(&d)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, d2) {
		b, _ = json.Marshal(d)
		t.Fatal("unequal ", d, d2)
	}

}
