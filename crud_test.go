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
	"moses/iotmodel"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func httpBaseRequest(auth string, method string, url string, body interface{}, result interface{}) (err error) {
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

func httpUserRequest(method string, url string, body interface{}, result interface{}) (err error) {
	auth := "Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICIzaUtabW9aUHpsMmRtQnBJdS1vSkY4ZVVUZHh4OUFIckVOcG5CcHM5SjYwIn0.eyJqdGkiOiJhOTBiMjY3Ny0xN2QzLTRlNTQtYjI0NS1iOGRjZGY0NzM0MWMiLCJleHAiOjE1MzkyNDM5MDEsIm5iZiI6MCwiaWF0IjoxNTM5MjQwMzAxLCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJiMzBlYWZkOC04MTA4LTQxZjktODVlZi1lOGY4MDM1MTk2ZTciLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJmcm9udGVuZCIsIm5vbmNlIjoiZjNmN2I3ZTUtNWFmYS00YzQ5LTlkODgtYTU2NzQ3YjlhNjQxIiwiYXV0aF90aW1lIjoxNTM5MjQwMzAwLCJzZXNzaW9uX3N0YXRlIjoiMjNmMGIxMzUtMDM0Yi00OGFmLWJjNDUtYjY5NzEzZmZhZTU4IiwiYWNyIjoiMSIsImFsbG93ZWQtb3JpZ2lucyI6WyIqIl0sInJlYWxtX2FjY2VzcyI6eyJyb2xlcyI6WyJkZXZlbG9wZXIiLCJ1bWFfYXV0aG9yaXphdGlvbiIsInVzZXIiXX0sInJlc291cmNlX2FjY2VzcyI6eyJhY2NvdW50Ijp7InJvbGVzIjpbIm1hbmFnZS1hY2NvdW50IiwibWFuYWdlLWFjY291bnQtbGlua3MiLCJ2aWV3LXByb2ZpbGUiXX19LCJyb2xlcyI6WyJ1bWFfYXV0aG9yaXphdGlvbiIsImRldmVsb3BlciIsInVzZXIiLCJvZmZsaW5lX2FjY2VzcyJdLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJmbG9yaWFuIiwiZW1haWwiOiJmZmZAdXNlci5kZSJ9.PO3ci63JSe_N6qj-McGdLqjg4TGawyLSKaNTbh05W6faz_5XRcEMGgn-xa-hXOGvcTOYubP_4W5Gwfutp-IG5Km9GIpTMJWx8LUTLh6jjBmPv15qYLW_hM5KSjpBR5lUX1e5bREedrnCucQ551KgJY2NPPDtlIgfAw7WTb1HQD3SIcbDaNTg2xzS2udKBUJwC1Q7eUJ12EV52u2Ih0p-ksW2jSLN_2-lnlC9Ht4Zw847YgsyKNTS0bRrxkz-wZVOeP9V5Lvj3aR5eZ7dU1Uin3_m5bZG99BSBKq9E4Hmv2KWx3GyRKl_Zu-RuziH1M9n-dnb5yBHeIX3IZWDTNrFcw"
	return httpBaseRequest(auth, method, url, body, result)
}

func httpAdminRequest(method string, url string, body interface{}, result interface{}) (err error) {
	auth := "Bearer eyJhbGciOiJSUzI1NiIsInR5cCIgOiAiSldUIiwia2lkIiA6ICIzaUtabW9aUHpsMmRtQnBJdS1vSkY4ZVVUZHh4OUFIckVOcG5CcHM5SjYwIn0.eyJqdGkiOiIyZTNkZTliMy0zNGFiLTQ3MDEtYjcwMS1mMzk1ZjkwMGI0MzQiLCJleHAiOjE1MzkyNjE4ODcsIm5iZiI6MCwiaWF0IjoxNTM5MjU4Mjg3LCJpc3MiOiJodHRwczovL2F1dGguc2VwbC5pbmZhaS5vcmcvYXV0aC9yZWFsbXMvbWFzdGVyIiwiYXVkIjoiZnJvbnRlbmQiLCJzdWIiOiJkZDY5ZWEwZC1mNTUzLTQzMzYtODBmMy03ZjQ1NjdmODVjN2IiLCJ0eXAiOiJCZWFyZXIiLCJhenAiOiJmcm9udGVuZCIsIm5vbmNlIjoiMWRjZTI2OTYtYjM5ZC00MmU0LTg3YzAtYjdiMmQwZjZhMDRiIiwiYXV0aF90aW1lIjoxNTM5MjU4Mjg2LCJzZXNzaW9uX3N0YXRlIjoiZDNlOGQ0NTUtYzEzZi00ZTc1LWIzYzQtN2NhZTE5OWE4MjZjIiwiYWNyIjoiMSIsImFsbG93ZWQtb3JpZ2lucyI6WyIqIl0sInJlYWxtX2FjY2VzcyI6eyJyb2xlcyI6WyJjcmVhdGUtcmVhbG0iLCJhZG1pbiIsImRldmVsb3BlciIsInVtYV9hdXRob3JpemF0aW9uIiwidXNlciJdfSwicmVzb3VyY2VfYWNjZXNzIjp7Im1hc3Rlci1yZWFsbSI6eyJyb2xlcyI6WyJ2aWV3LWlkZW50aXR5LXByb3ZpZGVycyIsInZpZXctcmVhbG0iLCJtYW5hZ2UtaWRlbnRpdHktcHJvdmlkZXJzIiwiaW1wZXJzb25hdGlvbiIsImNyZWF0ZS1jbGllbnQiLCJtYW5hZ2UtdXNlcnMiLCJxdWVyeS1yZWFsbXMiLCJ2aWV3LWF1dGhvcml6YXRpb24iLCJxdWVyeS1jbGllbnRzIiwicXVlcnktdXNlcnMiLCJtYW5hZ2UtZXZlbnRzIiwibWFuYWdlLXJlYWxtIiwidmlldy1ldmVudHMiLCJ2aWV3LXVzZXJzIiwidmlldy1jbGllbnRzIiwibWFuYWdlLWF1dGhvcml6YXRpb24iLCJtYW5hZ2UtY2xpZW50cyIsInF1ZXJ5LWdyb3VwcyJdfSwiYWNjb3VudCI6eyJyb2xlcyI6WyJtYW5hZ2UtYWNjb3VudCIsIm1hbmFnZS1hY2NvdW50LWxpbmtzIiwidmlldy1wcm9maWxlIl19fSwicm9sZXMiOlsidW1hX2F1dGhvcml6YXRpb24iLCJhZG1pbiIsImNyZWF0ZS1yZWFsbSIsImRldmVsb3BlciIsInVzZXIiLCJvZmZsaW5lX2FjY2VzcyJdLCJuYW1lIjoiU2VwbCBBZG1pbiIsInByZWZlcnJlZF91c2VybmFtZSI6InNlcGwiLCJnaXZlbl9uYW1lIjoiU2VwbCIsImZhbWlseV9uYW1lIjoiQWRtaW4iLCJlbWFpbCI6InNlcGxAc2VwbC5kZSJ9.G6eSBBHkpq-3tlemyqfSGmWbij3D4_p_MCb32XY9XDVgMya3GziU2Ki-6qqiZTsB9Nc5curQPl9bd5V9d9FR2-DXl2f7cr_dGVNRQ2BY5kvYZNT5HdQwx8xZfoKCZYX23UL1tyWj-nmi72URpBHUUXsNqrH5IBkJEEdp3Mkt4UeGjDTCSrFaq97lcJvoueYJ6bNXL0QQrDF5Q4aWYJlStdW-MlfK1IEjZmTEl8V2xRCyi4QNnOkTTXjIPbWP1sy8kiQcQIGdsMa6hhYxLuafdJEthMKZqaLLk1HMnnpWQp3_jD0zr_A7LdUGs8iMNuCpwfXqPznzFRySsWkHWjMWBQ"
	return httpBaseRequest(auth, method, url, body, result)
}

func TestCrud(t *testing.T) {
	worldCreate := CreateWorldRequest{Name: "World1", States: map[string]interface{}{"w1": "test"}}
	world := WorldMsg{}
	err := httpUserRequest("POST", integratedServer.URL+"/world", worldCreate, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldCreate.Name || !reflect.DeepEqual(world.States, worldCreate.States) {
		t.Fatal("unexpected create response", worldCreate, world)
	}

	worldUpdate := UpdateWorldRequest{Id: world.Id, Name: "World2", States: map[string]interface{}{"foo": "bar"}}
	world = WorldMsg{}
	err = httpUserRequest("PUT", integratedServer.URL+"/world", worldUpdate, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldUpdate.Name || !reflect.DeepEqual(world.States, worldUpdate.States) {
		t.Fatal("unexpected update response", worldUpdate, world)
	}

	world = WorldMsg{}
	err = httpUserRequest("GET", integratedServer.URL+"/world/"+worldUpdate.Id, nil, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || world.Name != worldUpdate.Name || !reflect.DeepEqual(world.States, worldUpdate.States) {
		t.Fatal("unexpected get response", worldUpdate, world)
	}

	roomCreate := CreateRoomRequest{World: world.Id, Name: "Room1", States: map[string]interface{}{"r1": "test"}}
	room := RoomResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/room", roomCreate, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomCreate.Name || !reflect.DeepEqual(room.Room.States, roomCreate.States) {
		t.Fatal("unexpected create response", roomCreate, room)
	}

	roomUpdate := UpdateRoomRequest{Id: room.Room.Id, Name: "Room2", States: map[string]interface{}{"foo": "bar2"}}
	room = RoomResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/room", roomUpdate, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomUpdate.Name || !reflect.DeepEqual(room.Room.States, roomUpdate.States) {
		t.Fatal("unexpected update response", roomUpdate, room)
	}

	room = RoomResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/room/"+roomUpdate.Id, nil, &room)
	if err != nil {
		t.Fatal("Error", err)
	}
	if room.World == "" || room.Room.Id == "" || room.Room.Name != roomUpdate.Name || !reflect.DeepEqual(room.Room.States, roomUpdate.States) {
		t.Fatal("unexpected get response", roomUpdate, room)
	}

	deviceCreate := CreateDeviceRequest{Room: room.Room.Id, Name: "Device1", States: map[string]interface{}{"d1": "test"}}
	device := DeviceResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/device", deviceCreate, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceCreate.Name || !reflect.DeepEqual(device.Device.States, deviceCreate.States) {
		t.Fatal("unexpected create response", deviceCreate, device)
	}

	deviceUpdate := UpdateDeviceRequest{Id: device.Device.Id, Name: "Device2", States: map[string]interface{}{"foo": "bar3"}}
	device = DeviceResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/device", deviceUpdate, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceUpdate.Name || !reflect.DeepEqual(device.Device.States, deviceUpdate.States) {
		t.Fatal("unexpected update response", deviceUpdate, device)
	}

	device = DeviceResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/device/"+deviceUpdate.Id, nil, &device)
	if err != nil {
		t.Fatal("Error", err)
	}
	if device.World == "" || device.Device.Id == "" || device.Device.Name != deviceUpdate.Name || !reflect.DeepEqual(device.Device.States, deviceUpdate.States) {
		t.Fatal("unexpected get response", deviceUpdate, device)
	}

	serviceCreate := CreateServiceRequest{Device: device.Device.Id, Name: "Service1", Code: "var foo = \"bar\"", SensorInterval: 1 * time.Second}
	service := ServiceResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/service", serviceCreate, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceCreate.Name || service.Service.Code != serviceCreate.Code || service.Service.SensorInterval != serviceCreate.SensorInterval {
		t.Fatal("unexpected create response", serviceCreate, service)
	}

	serviceUpdate := UpdateServiceRequest{Id: service.Service.Id, Name: "Service2", Code: "var foo = \"42\"", SensorInterval: 2 * time.Second}
	service = ServiceResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/service", serviceUpdate, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceUpdate.Name || service.Service.Code != serviceUpdate.Code || service.Service.SensorInterval != serviceUpdate.SensorInterval {
		t.Fatal("unexpected update response", serviceUpdate, service)
	}

	service = ServiceResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/service/"+serviceUpdate.Id, nil, &service)
	if err != nil {
		t.Fatal("Error", err)
	}
	if service.World == "" || service.Service.Id == "" || service.Service.Name != serviceUpdate.Name || service.Service.Code != serviceUpdate.Code || service.Service.SensorInterval != serviceUpdate.SensorInterval {
		t.Fatal("unexpected get response", serviceUpdate, service)
	}

	//routines

	worldRoutineCreate := CreateChangeRoutineRequest{RefType: "world", RefId: world.Id, Code: "var foo = 'world'", Interval: 3 * time.Second}
	worldRoutine := ChangeRoutineResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/changeroutine", worldRoutineCreate, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id == "" || worldRoutine.Code != worldRoutineCreate.Code || worldRoutine.Interval != worldRoutineCreate.Interval || worldRoutine.RefId != worldRoutineCreate.RefId || worldRoutine.RefType != worldRoutineCreate.RefType {
		t.Fatal("unexpected create response", worldRoutine, worldRoutineCreate)
	}

	worldRoutineUpdate := UpdateChangeRoutineRequest{Id: worldRoutine.Id, Code: "var foo = 'world2'", Interval: 4 * time.Second}
	worldRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/changeroutine", worldRoutineUpdate, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id != worldRoutineUpdate.Id || worldRoutine.Code != worldRoutineUpdate.Code || worldRoutine.Interval != worldRoutineUpdate.Interval {
		t.Fatal("unexpected update response", worldRoutineUpdate, worldRoutine)
	}

	worldRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/changeroutine/"+worldRoutineUpdate.Id, nil, &worldRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldRoutine.Id != worldRoutineUpdate.Id || worldRoutine.Code != worldRoutineUpdate.Code || worldRoutine.Interval != worldRoutineUpdate.Interval {
		t.Fatal("unexpected update response", worldRoutine, worldRoutineUpdate)
	}

	roomRoutineCreate := CreateChangeRoutineRequest{RefType: "room", RefId: room.Room.Id, Code: "var foo = 'room'", Interval: 3 * time.Second}
	roomRoutine := ChangeRoutineResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/changeroutine", roomRoutineCreate, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id == "" || roomRoutine.Code != roomRoutineCreate.Code || roomRoutine.Interval != roomRoutineCreate.Interval || roomRoutine.RefId != roomRoutineCreate.RefId || roomRoutine.RefType != roomRoutineCreate.RefType {
		t.Fatal("unexpected create response", roomRoutineCreate, roomRoutine)
	}

	roomRoutineUpdate := UpdateChangeRoutineRequest{Id: roomRoutine.Id, Code: "var foo = 'room2'", Interval: 4 * time.Second}
	roomRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/changeroutine", roomRoutineUpdate, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id != roomRoutineUpdate.Id || roomRoutine.Code != roomRoutineUpdate.Code || roomRoutine.Interval != roomRoutineUpdate.Interval {
		t.Fatal("unexpected update response", roomRoutine, roomRoutineUpdate)
	}

	roomRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/changeroutine/"+roomRoutineUpdate.Id, nil, &roomRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if roomRoutine.Id != roomRoutineUpdate.Id || roomRoutine.Code != roomRoutineUpdate.Code || roomRoutine.Interval != roomRoutineUpdate.Interval {
		t.Fatal("unexpected update response", roomRoutineUpdate, roomRoutine)
	}

	deviceRoutineCreate := CreateChangeRoutineRequest{RefType: "device", RefId: device.Device.Id, Code: "var foo = 'device'", Interval: 3 * time.Second}
	deviceRoutine := ChangeRoutineResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/changeroutine", deviceRoutineCreate, &deviceRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceRoutine.Id == "" || deviceRoutine.Code != deviceRoutineCreate.Code || deviceRoutine.Interval != deviceRoutineCreate.Interval || deviceRoutine.RefId != deviceRoutineCreate.RefId || deviceRoutine.RefType != deviceRoutineCreate.RefType {
		t.Fatal("unexpected create response", deviceRoutine, deviceRoutineCreate)
	}

	deviceRoutineUpdate := UpdateChangeRoutineRequest{Id: deviceRoutine.Id, Code: "var foo = 'device2'", Interval: 4 * time.Second}
	deviceRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("PUT", integratedServer.URL+"/changeroutine", deviceRoutineUpdate, &deviceRoutine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceRoutine.Id != deviceRoutineUpdate.Id || deviceRoutine.Code != deviceRoutineUpdate.Code || deviceRoutine.Interval != deviceRoutineUpdate.Interval {
		t.Fatal("unexpected update response", deviceRoutine, deviceRoutineUpdate)
	}

	deviceRoutine = ChangeRoutineResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/changeroutine/"+deviceRoutineUpdate.Id, nil, &deviceRoutine)
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
	err = httpUserRequest("GET", integratedServer.URL+"/device/"+deviceUpdate.Id, nil, &device)
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
	err = httpUserRequest("GET", integratedServer.URL+"/room/"+roomUpdate.Id, nil, &room)
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
	err = httpUserRequest("GET", integratedServer.URL+"/world/"+worldUpdate.Id, nil, &world)
	if err != nil {
		t.Fatal("Error", err)
	}
	if world.Id == "" || !reflect.DeepEqual(world, expectedWorld) {
		t.Fatal("unexpected get response", expectedWorld, world)
	}

	worlds := []WorldMsg{}
	err = httpUserRequest("GET", integratedServer.URL+"/worlds", nil, &worlds)
	if err != nil {
		t.Fatal("Error", err)
	}
	if !reflect.DeepEqual(worlds, []WorldMsg{expectedWorld}) {
		t.Fatal("unexpected get response", expectedWorld, worlds)
	}

	//iot integration

	dt, err := helperCreateDeviceType()
	if err != nil {
		t.Fatal("Error", err)
	}

	time.Sleep(4 * time.Second)

	createByDt := CreateDeviceByTypeRequest{Room: room.Room.Id, ExternalRef: "foobar", Name: "foo42", DeviceTypeId: dt.Id}
	device = DeviceResponse{}
	err = httpUserRequest("POST", integratedServer.URL+"/device/bydevicetype", createByDt, &device)
	if err != nil {
		t.Fatal("Error", err)
	}

	expectedCode := `
/* {
	"a": 0
}
 */
var input = moses.service.input; 
        
var output = {
	"b": "STRING"
}

moses.service.send(output);
`

	serviceResult := Service{}
	for _, service := range device.Device.Services {
		serviceResult = service
	}

	if device.World == "" || device.Device.Id == "" || device.Device.Name != createByDt.Name {
		t.Fatal("unexpected create response", device.World, device.Device.Id, device.Device.Name)
	}
	if strings.Join(strings.Fields(serviceResult.Code), "") != strings.Join(strings.Fields(expectedCode), "") {
		t.Fatal("unexpected code", "::"+strings.Join(strings.Fields(serviceResult.Code), "")+"::"+strings.Join(strings.Fields(expectedCode), "")+"::")
	}

	time.Sleep(3 * time.Second)

	iotDevice := iotmodel.DeviceInstance{}
	err = httpUserRequest("GET", integratedConfig.IotUrl+"/deviceInstance/"+url.PathEscape(device.Device.ExternalRef), nil, &iotDevice)
	if err != nil {
		t.Fatal("Error", integratedConfig.IotUrl+"/deviceInstance/"+url.PathEscape(device.Device.ExternalRef), err)
	}
	if device.Device.ExternalRef != iotDevice.Id || iotDevice.Name != createByDt.Name {
		t.Fatal("unexpected response", device.Device.ExternalRef, iotDevice.Id, iotDevice.Name, createByDt.Name)
	}

	deviceGet := DeviceResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/device/"+device.Device.Id, nil, &deviceGet)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceGet.World == "" || deviceGet.Device.Id == "" || !reflect.DeepEqual(deviceGet, device) {
		t.Fatal("unexpected get response", deviceGet, device)
	}

	templateCreate := CreateTemplateRequest{Name: "testName", Template: "var foo = \"{{bar}}\"", Description: "testDesc"}
	template := RoutineTemplate{}
	err = httpAdminRequest("POST", integratedServer.URL+"/routinetemplate", templateCreate, &template)
	if err != nil {
		t.Fatal("Error", err)
	}
	if template.Name != templateCreate.Name || template.Template != templateCreate.Template || template.Description != templateCreate.Description || len(template.Parameter) != 1 || template.Parameter[0] != "bar" {
		t.Fatal("unexpected create response", templateCreate, template)
	}

	templateUpdate := UpdateTemplateRequest{Id: template.Id, Description: "testDesc", Template: "var foo = \"{{foobar}}\"", Name: "testName"}
	template = RoutineTemplate{}
	err = httpAdminRequest("PUT", integratedServer.URL+"/routinetemplate", templateUpdate, &template)
	if err != nil {
		t.Fatal("Error", err, templateUpdate)
	}
	if template.Name != templateUpdate.Name || template.Template != templateUpdate.Template || template.Description != templateUpdate.Description || len(template.Parameter) != 1 || template.Parameter[0] != "foobar" {
		t.Fatal("unexpected update response", templateUpdate, template)
	}

	template = RoutineTemplate{}
	err = httpUserRequest("GET", integratedServer.URL+"/routinetemplate/"+templateUpdate.Id, nil, &template)
	if err != nil {
		t.Fatal("Error", err)
	}
	if template.Name != templateUpdate.Name || template.Template != templateUpdate.Template || template.Description != templateUpdate.Description || len(template.Parameter) != 1 || template.Parameter[0] != "foobar" {
		t.Fatal("unexpected get response", templateUpdate, template)
	}

	routine := ChangeRoutineResponse{}
	templUse := CreateChangeRoutineByTemplateRequest{TemplId: template.Id, Interval: 1 * time.Second, Parameter: map[string]string{"foobar": "42"}, RefType: "device", RefId: deviceGet.Device.Id}
	err = httpUserRequest("POST", integratedServer.URL+"/usetemplate", templUse, &routine)
	if err != nil {
		t.Fatal("Error", err)
	}
	if routine.Id == "" || routine.Code != "var foo = \"42\"" || routine.Interval != templUse.Interval || templUse.RefType != "device" || templUse.RefId != deviceGet.Device.Id {
		t.Fatal("unexpected response", routine)
	}

	deviceGet.Device.ChangeRoutines = map[string]ChangeRoutine{}
	deviceGet.Device.ChangeRoutines[routine.Id] = ChangeRoutine{Id: routine.Id, Interval: routine.Interval, Code: routine.Code}
	deviceGet2 := DeviceResponse{}
	err = httpUserRequest("GET", integratedServer.URL+"/device/"+deviceGet.Device.Id, nil, &deviceGet2)
	if err != nil {
		t.Fatal("Error", err)
	}
	if deviceGet2.World == "" || deviceGet2.Device.Id == "" || !reflect.DeepEqual(deviceGet2, deviceGet) {
		t.Fatal("unexpected get response", deviceGet2, deviceGet)
	}

	//delete
	createWorldToDelete := CreateWorldRequest{Name: "WorldToDelete"}
	worldToDelete := WorldMsg{}
	err = httpUserRequest("POST", integratedServer.URL+"/world", createWorldToDelete, &worldToDelete)
	if err != nil {
		t.Fatal("Error", err)
	}
	if worldToDelete.Id == "" || worldToDelete.Name != createWorldToDelete.Name {
		t.Fatal("unexpected create response", worldToDelete)
	}

	err = httpUserRequest("DELETE", integratedServer.URL+"/world/"+worldToDelete.Id, nil, nil)
	if err != nil {
		t.Fatal("Error", err)
	}

	worlds = []WorldMsg{}
	err = httpUserRequest("GET", integratedServer.URL+"/worlds", nil, &worlds)
	if err != nil {
		t.Fatal("Error", err)
	}
	if len(worlds) != 1 || worlds[0].Name == worldToDelete.Name {
		t.Fatal("unexpected get response", worlds)
	}

	//TODO:
	// read and use default template
	// delete entities
}

func helperCreateDeviceType() (dt iotmodel.DeviceType, err error) {
	protocol := iotmodel.Protocol{}
	err = httpAdminRequest("GET", integratedServer.URL+"/admin/initiot", nil, &protocol)
	if err != nil {
		return
	}
	dtStr := `{  
   "name":"test",
   "description":"test",
   "device_class":{  
      "id":"iot#71bd0260-56bd-4cb0-9797-862d52aee21e"
   },
   "services":[  
      {  
         "service_type":"http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#Actuator",
         "name":"actuator",
         "description":"test",
         "protocol":{  
            "id":"` + protocol.Id + `"
         },
         "input":[  
            {  
               "name":"in",
               "msg_segment":{  
                  "id":"` + protocol.MsgStructure[0].Id + `"
               },
               "type":{  
                  "name":"testin",
                  "description":"test",
                  "base_type":"http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#structure",
                  "fields":[  
                     {  
                        "name":"a",
                        "type":{  
                           "name":"test_int",
                           "description":"test_int",
                           "base_type":"http://www.w3.org/2001/XMLSchema#integer",
                           "fields":null,
                           "literal":""
                        }
                     }
                  ],
                  "literal":""
               },
               "format":"http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#json",
               "additional_formatinfo":[]
            }
         ],
		 "output":[  
            {  
               "name":"out",
               "msg_segment":{  
                  "id":"` + protocol.MsgStructure[0].Id + `"
               },
               "type":{  
                  "name":"testout",
                  "description":"test",
                  "base_type":"http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#structure",
                  "fields":[  
                     {  
                        "name":"b",
                        "type":{  
                           "name":"test_str",
                           "description":"test_str",
                           "base_type":"http://www.w3.org/2001/XMLSchema#string",
                           "fields":null,
                           "literal":""
                        }
                     }
                  ],
                  "literal":""
               },
               "format":"http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#json",
               "additional_formatinfo":[]
            }
         ],
         "url":"test"
      }
   ],
   "vendor":{  
      "id":"iot#24e5bb75-6d18-4e4e-87eb-ea4e554a14fb"
   }
}`
	var body interface{}
	err = json.Unmarshal([]byte(dtStr), &body)
	if err != nil {
		return
	}
	err = httpUserRequest("POST", integratedConfig.IotUrl+"/deviceType", body, &dt)
	return
}
