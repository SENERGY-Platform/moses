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
	"errors"
	"io/ioutil"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func httpRequest(method string, url string, body interface{}, result interface{}) (err error) {
	auth := "Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICIzaUtabW9aUHpsMmRtQnBJdS1vSkY4ZVVUZHh4OUFIckVOcG5CcHM5SjYwIn0.eyJqdGkiOiJhOTBiMjY3Ny0xN2QzLTRlNTQtYjI0NS1iOGRjZGY0NzM0MWMiLCJleHAiOjE1MzkyNDM5MDEsIm5iZiI6MCwiaWF0IjoxNTM5MjQwMzAxLCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJiMzBlYWZkOC04MTA4LTQxZjktODVlZi1lOGY4MDM1MTk2ZTciLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJmcm9udGVuZCIsIm5vbmNlIjoiZjNmN2I3ZTUtNWFmYS00YzQ5LTlkODgtYTU2NzQ3YjlhNjQxIiwiYXV0aF90aW1lIjoxNTM5MjQwMzAwLCJzZXNzaW9uX3N0YXRlIjoiMjNmMGIxMzUtMDM0Yi00OGFmLWJjNDUtYjY5NzEzZmZhZTU4IiwiYWNyIjoiMSIsImFsbG93ZWQtb3JpZ2lucyI6WyIqIl0sInJlYWxtX2FjY2VzcyI6eyJyb2xlcyI6WyJkZXZlbG9wZXIiLCJ1bWFfYXV0aG9yaXphdGlvbiIsInVzZXIiXX0sInJlc291cmNlX2FjY2VzcyI6eyJhY2NvdW50Ijp7InJvbGVzIjpbIm1hbmFnZS1hY2NvdW50IiwibWFuYWdlLWFjY291bnQtbGlua3MiLCJ2aWV3LXByb2ZpbGUiXX19LCJyb2xlcyI6WyJ1bWFfYXV0aG9yaXphdGlvbiIsImRldmVsb3BlciIsInVzZXIiLCJvZmZsaW5lX2FjY2VzcyJdLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJmbG9yaWFuIiwiZW1haWwiOiJmZmZAdXNlci5kZSJ9.PO3ci63JSe_N6qj-McGdLqjg4TGawyLSKaNTbh05W6faz_5XRcEMGgn-xa-hXOGvcTOYubP_4W5Gwfutp-IG5Km9GIpTMJWx8LUTLh6jjBmPv15qYLW_hM5KSjpBR5lUX1e5bREedrnCucQ551KgJY2NPPDtlIgfAw7WTb1HQD3SIcbDaNTg2xzS2udKBUJwC1Q7eUJ12EV52u2Ih0p-ksW2jSLN_2-lnlC9Ht4Zw847YgsyKNTS0bRrxkz-wZVOeP9V5Lvj3aR5eZ7dU1Uin3_m5bZG99BSBKq9E4Hmv2KWx3GyRKl_Zu-RuziH1M9n-dnb5yBHeIX3IZWDTNrFcw"
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var req *http.Request
	if body != nil {
		req, err = http.NewRequest(method, url, strings.NewReader(string(b)))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return errors.New("unexpected response: " + resp.Status + " " + string(body))
	}
	if result != nil {
		err = json.NewDecoder(resp.Body).Decode(result)
	}
	return
}

func TestSimpleCrud(t *testing.T) {
	worldCreate := CreateWorldRequest{Name: "World1", States: map[string]interface{}{"w1": "test"}}
	world := WorldMsg{}
	err := httpRequest("POST", integratedServer.URL+"/world", worldCreate, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldCreate.Name || !reflect.DeepEqual(world.States, worldCreate.States) {
		t.Fatal("unexpected create response", worldCreate, world)
	}

	worldUpdate := UpdateWorldRequest{Id: world.Id, Name: "World2", States: map[string]interface{}{"foo": "bar"}}
	world = WorldMsg{}
	err = httpRequest("PUT", integratedServer.URL+"/world", worldUpdate, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldUpdate.Name || !reflect.DeepEqual(world.States, worldUpdate.States) {
		t.Fatal("unexpected update response", worldUpdate, world)
	}

	world = WorldMsg{}
	err = httpRequest("GET", integratedServer.URL+"/world/"+worldUpdate.Id, nil, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldUpdate.Name || !reflect.DeepEqual(world.States, worldUpdate.States) {
		t.Fatal("unexpected get response", worldUpdate, world)
	}

	roomCreate := CreateRoomRequest{World: world.Id, Name: "Room1", States: map[string]interface{}{"r1": "test"}}
	room := RoomResponse{}
	err = httpRequest("POST", integratedServer.URL+"/room", roomCreate, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomCreate.Name || !reflect.DeepEqual(room.Room.States, roomCreate.States) {
		t.Fatal("unexpected create response", roomCreate, room)
	}

	roomUpdate := UpdateRoomRequest{Id: room.Room.Id, Name: "Room2", States: map[string]interface{}{"foo": "bar2"}}
	room = RoomResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/room", roomUpdate, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomUpdate.Name || !reflect.DeepEqual(room.Room.States, roomUpdate.States) {
		t.Fatal("unexpected update response", roomUpdate, room)
	}

	room = RoomResponse{}
	err = httpRequest("GET", integratedServer.URL+"/room/"+roomUpdate.Id, nil, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomUpdate.Name || !reflect.DeepEqual(room.Room.States, roomUpdate.States) {
		t.Fatal("unexpected get response", roomUpdate, room)
	}

	deviceCreate := CreateDeviceRequest{Room: room.Room.Id, Name: "Device1", States: map[string]interface{}{"d1": "test"}}
	device := DeviceResponse{}
	err = httpRequest("POST", integratedServer.URL+"/device", deviceCreate, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceCreate.Name || !reflect.DeepEqual(device.Device.States, deviceCreate.States) {
		t.Fatal("unexpected create response", deviceCreate, device)
	}

	deviceUpdate := UpdateDeviceRequest{Id: device.Device.Id, Name: "Device2", States: map[string]interface{}{"foo": "bar3"}}
	device = DeviceResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/device", deviceUpdate, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceUpdate.Name || !reflect.DeepEqual(device.Device.States, deviceUpdate.States) {
		t.Fatal("unexpected update response", deviceUpdate, device)
	}

	device = DeviceResponse{}
	err = httpRequest("GET", integratedServer.URL+"/device/"+deviceUpdate.Id, nil, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceUpdate.Name || !reflect.DeepEqual(device.Device.States, deviceUpdate.States) {
		t.Fatal("unexpected get response", deviceUpdate, device)
	}

	//get world by id
	//compare world with gotten one
	//add room
	//compare room output with input
	//get room by id & compare
	//update room & compare result
	//add device
	//compare device output with input
	//get device by id
	//compare device with gotten one
	//add change routines to all levels
	//get world list and compare deep
	//get world by id and compare deep
	//get room by id and compare deep
	//get device by id and compare deep
}
