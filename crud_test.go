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
	"time"
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

	serviceCreate := CreateServiceRequest{Device: device.Device.Id, Name: "Service1", Code: "var foo = \"bar\"", SensorInterval: 1 * time.Second}
	service := ServiceResponse{}
	err = httpRequest("POST", integratedServer.URL+"/service", serviceCreate, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceCreate.Name || service.Service.Code != serviceCreate.Code || service.Service.SensorInterval != serviceCreate.SensorInterval {
		t.Fatal("unexpected create response", serviceCreate, service)
	}

	serviceUpdate := UpdateServiceRequest{Id: service.Service.Id, Name: "Service2", Code: "var foo = \"42\"", SensorInterval: 2 * time.Second}
	service = ServiceResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/service", serviceUpdate, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceUpdate.Name || service.Service.Code != serviceUpdate.Code || service.Service.SensorInterval != serviceUpdate.SensorInterval {
		t.Fatal("unexpected update response", serviceUpdate, service)
	}

	service = ServiceResponse{}
	err = httpRequest("GET", integratedServer.URL+"/service/"+serviceUpdate.Id, nil, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceUpdate.Name || service.Service.Code != serviceUpdate.Code || service.Service.SensorInterval != serviceUpdate.SensorInterval {
		t.Fatal("unexpected get response", serviceUpdate, service)
	}

	//routines

	worldRoutineCreate := CreateChangeRoutineRequest{RefType: "world", RefId: world.Id, Code: "var foo = 'world'", Interval: 3 * time.Second}
	worldRoutine := ChangeRoutineResponse{}
	err = httpRequest("POST", integratedServer.URL+"/changeroutine", worldRoutineCreate, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id == "" || worldRoutine.Code != worldRoutineCreate.Code || worldRoutine.Interval != worldRoutineCreate.Interval || worldRoutine.RefId != worldRoutineCreate.RefId || worldRoutine.RefType != worldRoutineCreate.RefType {
		t.Fatal("unexpected create response", worldRoutine, worldRoutineCreate)
	}

	worldRoutineUpdate := UpdateChangeRoutineRequest{Id: worldRoutine.Id, Code: "var foo = 'world2'", Interval: 4 * time.Second}
	worldRoutine = ChangeRoutineResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/changeroutine", worldRoutineUpdate, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id != worldRoutineUpdate.Id || worldRoutine.Code != worldRoutineUpdate.Code || worldRoutine.Interval != worldRoutineUpdate.Interval {
		t.Fatal("unexpected update response", worldRoutineUpdate, worldRoutine)
	}

	worldRoutine = ChangeRoutineResponse{}
	err = httpRequest("GET", integratedServer.URL+"/changeroutine/"+worldRoutineUpdate.Id, nil, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id != worldRoutineUpdate.Id || worldRoutine.Code != worldRoutineUpdate.Code || worldRoutine.Interval != worldRoutineUpdate.Interval {
		t.Fatal("unexpected update response", worldRoutine, worldRoutineUpdate)
	}

	roomRoutineCreate := CreateChangeRoutineRequest{RefType: "room", RefId: room.Room.Id, Code: "var foo = 'room'", Interval: 3 * time.Second}
	roomRoutine := ChangeRoutineResponse{}
	err = httpRequest("POST", integratedServer.URL+"/changeroutine", roomRoutineCreate, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id == "" || roomRoutine.Code != roomRoutineCreate.Code || roomRoutine.Interval != roomRoutineCreate.Interval || roomRoutine.RefId != roomRoutineCreate.RefId || roomRoutine.RefType != roomRoutineCreate.RefType {
		t.Fatal("unexpected create response", roomRoutineCreate, roomRoutine)
	}

	roomRoutineUpdate := UpdateChangeRoutineRequest{Id: roomRoutine.Id, Code: "var foo = 'room2'", Interval: 4 * time.Second}
	roomRoutine = ChangeRoutineResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/changeroutine", roomRoutineUpdate, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id != roomRoutineUpdate.Id || roomRoutine.Code != roomRoutineUpdate.Code || roomRoutine.Interval != roomRoutineUpdate.Interval {
		t.Fatal("unexpected update response", roomRoutine, roomRoutineUpdate)
	}

	roomRoutine = ChangeRoutineResponse{}
	err = httpRequest("GET", integratedServer.URL+"/changeroutine/"+roomRoutineUpdate.Id, nil, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id != roomRoutineUpdate.Id || roomRoutine.Code != roomRoutineUpdate.Code || roomRoutine.Interval != roomRoutineUpdate.Interval {
		t.Fatal("unexpected update response", roomRoutineUpdate, roomRoutine)
	}

	deviceRoutineCreate := CreateChangeRoutineRequest{RefType: "device", RefId: device.Device.Id, Code: "var foo = 'device'", Interval: 3 * time.Second}
	deviceRoutine := ChangeRoutineResponse{}
	err = httpRequest("POST", integratedServer.URL+"/changeroutine", deviceRoutineCreate, &deviceRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceRoutine.Id == "" || deviceRoutine.Code != deviceRoutineCreate.Code || deviceRoutine.Interval != deviceRoutineCreate.Interval || deviceRoutine.RefId != deviceRoutineCreate.RefId || deviceRoutine.RefType != deviceRoutineCreate.RefType {
		t.Fatal("unexpected create response", deviceRoutine, deviceRoutineCreate)
	}

	deviceRoutineUpdate := UpdateChangeRoutineRequest{Id: deviceRoutine.Id, Code: "var foo = 'device2'", Interval: 4 * time.Second}
	deviceRoutine = ChangeRoutineResponse{}
	err = httpRequest("PUT", integratedServer.URL+"/changeroutine", deviceRoutineUpdate, &deviceRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceRoutine.Id != deviceRoutineUpdate.Id || deviceRoutine.Code != deviceRoutineUpdate.Code || deviceRoutine.Interval != deviceRoutineUpdate.Interval {
		t.Fatal("unexpected update response", deviceRoutine, deviceRoutineUpdate)
	}

	deviceRoutine = ChangeRoutineResponse{}
	err = httpRequest("GET", integratedServer.URL+"/changeroutine/"+deviceRoutineUpdate.Id, nil, &deviceRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceRoutine.Id != deviceRoutineUpdate.Id || deviceRoutine.Code != deviceRoutineUpdate.Code || deviceRoutine.Interval != deviceRoutineUpdate.Interval {
		t.Fatal("unexpected update response", deviceRoutineUpdate, deviceRoutine)
	}

	//deep

	expectedDevice := DeviceResponse{World: world.Id, Room: room.Room.Id, Device: device.Device}
	expectedDevice.Device.ChangeRoutines = map[string]ChangeRoutine{deviceRoutine.Id: ChangeRoutine{Id: deviceRoutine.Id, Interval: deviceRoutine.Interval, Code: deviceRoutine.Code}}
	expectedDevice.Device.Services = map[string]Service{service.Service.Id: Service{Id: service.Service.Id, SensorInterval: service.Service.SensorInterval, Code: service.Service.Code, Name: service.Service.Name, ExternalRef: service.Service.ExternalRef}}
	device = DeviceResponse{}
	err = httpRequest("GET", integratedServer.URL+"/device/"+deviceUpdate.Id, nil, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if !reflect.DeepEqual(device, expectedDevice) {
		t.Fatal("unexpected get response", expectedDevice, device)
	}

	expectedRoom := RoomResponse{World: world.Id, Room: room.Room}
	expectedRoom.Room.ChangeRoutines = map[string]ChangeRoutine{roomRoutine.Id: ChangeRoutine{Id: roomRoutine.Id, Interval: roomRoutine.Interval, Code: roomRoutine.Code}}
	expectedRoom.Room.Devices = map[string]DeviceMsg{device.Device.Id: device.Device}
	room = RoomResponse{}
	err = httpRequest("GET", integratedServer.URL+"/room/"+roomUpdate.Id, nil, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || !reflect.DeepEqual(room, expectedRoom) {
		t.Fatal("unexpected get response", expectedRoom, room)
	}

	expectedWorld := world
	expectedWorld.ChangeRoutines = map[string]ChangeRoutine{worldRoutine.Id: ChangeRoutine{Id: worldRoutine.Id, Interval: worldRoutine.Interval, Code: worldRoutine.Code}}
	expectedWorld.Rooms = map[string]RoomMsg{expectedRoom.Room.Id: expectedRoom.Room}
	world = WorldMsg{}
	err = httpRequest("GET", integratedServer.URL+"/world/"+worldUpdate.Id, nil, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || !reflect.DeepEqual(world, expectedWorld) {
		t.Fatal("unexpected get response", expectedWorld, world)
	}

	worlds := []WorldMsg{}
	err = httpRequest("GET", integratedServer.URL+"/worlds", nil, &worlds)
	if err != nil {
		t.Fatal("Error", err)
	}
	if !reflect.DeepEqual(worlds, []WorldMsg{expectedWorld}) {
		t.Fatal("unexpected get response", expectedWorld, worlds)
	}

	//add change routines to all levels & compare

}
