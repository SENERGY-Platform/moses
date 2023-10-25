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
	"encoding/json"
	"log"
	"runtime/debug"
	"sync"
)

type CreateWorldRequest struct {
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type UpdateWorldRequest struct {
	Id             string                   `json:"id"`
	Name           string                   `json:"name"`
	States         map[string]interface{}   `json:"states"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
}

type RoomResponse struct {
	World string  `json:"world"`
	Room  RoomMsg `json:"room"`
}

type UpdateRoomRequest struct {
	Id             string                   `json:"id"`
	Name           string                   `json:"name"`
	States         map[string]interface{}   `json:"states"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
}

type CreateRoomRequest struct {
	World  string                 `json:"world"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type DeviceResponse struct {
	World  string    `json:"world"`
	Room   string    `json:"room"`
	Device DeviceMsg `json:"device"`
}

type UpdateDeviceRequest struct {
	Id             string                   `json:"id"`
	Name           string                   `json:"name"`
	States         map[string]interface{}   `json:"states"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
	Services       map[string]Service       `json:"services"`
	ExternalRef    string                   `json:"external_ref"` //platform intern device id; 1:1
}

type CreateDeviceRequest struct {
	Room        string                 `json:"room"`
	Name        string                 `json:"name"`
	States      map[string]interface{} `json:"states"`
	ExternalRef string                 `json:"external_ref"` //platform intern device id; 1:1
}

type CreateDeviceByTypeRequest struct {
	DeviceTypeId string `json:"device_type_id"`
	Room         string `json:"room"`
	Name         string `json:"name"`
}

type UpdateServiceRequest struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	ExternalRef    string `json:"external_ref"` //platform intern service id
	SensorInterval int64  `json:"sensor_interval"`
	Code           string `json:"code"`
}

type ServiceResponse struct {
	World   string               `json:"world"`
	Room    string               `json:"room"`
	Device  string               `json:"device"`
	Service UpdateServiceRequest `json:"service"`
}

type CreateServiceRequest struct {
	Device         string `json:"device"`
	Name           string `json:"name"`
	ExternalRef    string `json:"external_ref"` //platform intern service id, will be used to populate Service.Marshaller and as endpoint for the Connector
	SensorInterval int64  `json:"sensor_interval"`
	Code           string `json:"code"`
}

// {ref_type:"workd|room|device", ref_id: "", interval: 0, code:""}
type CreateChangeRoutineRequest struct {
	RefType  string `json:"ref_type"` // "world" || "room" || "device"
	RefId    string `json:"ref_id"`
	Interval int64  `json:"interval"`
	Code     string `json:"code"`
}

type UpdateChangeRoutineRequest struct {
	Id       string `json:"id"`
	Interval int64  `json:"interval"`
	Code     string `json:"code"`
}

type ChangeRoutineResponse struct {
	Id       string `json:"id"`
	RefType  string `json:"ref_type"` // "world" || "room" || "device"
	RefId    string `json:"ref_id"`
	Interval int64  `json:"interval"`
	Code     string `json:"code"`
}

type CreateTemplateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
}

type UpdateTemplateRequest struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
}

// {ref_type:"workd|room|device", ref_id: "", templ_id: "", name: "", desc: "", parameter: {<<param_name>>: <<param_value>>}}
type CreateChangeRoutineByTemplateRequest struct {
	RefType   string            `json:"ref_type"` // "world" || "room" || "device"
	RefId     string            `json:"ref_id"`
	TemplId   string            `json:"templ_id"`
	Interval  int64             `json:"interval"`
	Parameter map[string]string `json:"parameter"`
}

type UpdateChangeRoutineByTemplateRequest struct {
	RoutineId string            `json:"routine_id""`
	TemplId   string            `json:"templ_id"`
	Interval  int64             `json:"interval"`
	Parameter map[string]string `json:"parameter"`
}

// msg variants of model without pointers for thread safety

type WorldMsg struct {
	Id             string                   `json:"id"`
	Owner          string                   `json:"-"`
	Name           string                   `json:"name"`
	States         map[string]interface{}   `json:"states"`
	Rooms          map[string]RoomMsg       `json:"rooms"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
}

type RoomMsg struct {
	Id             string                   `json:"id"`
	Name           string                   `json:"name"`
	States         map[string]interface{}   `json:"states"`
	Devices        map[string]DeviceMsg     `json:"devices"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
}

type DeviceMsg struct {
	Id             string                   `json:"id"`
	Name           string                   `json:"name"`
	ExternalTypeId string                   `json:"external_type_id"`
	ExternalRef    string                   `json:"external_ref"` //platform intern device id; 1:1
	States         map[string]interface{}   `json:"states"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines"`
	Services       map[string]Service       `json:"services"`
}

func jsonCopy(from interface{}, to interface{}) (err error) {
	temp, err := json.Marshal(from)
	if err != nil {
		log.Printf("ERROR: %#v \n", from)
		debug.PrintStack()
		return err
	}
	err = json.Unmarshal(temp, to)
	if err != nil {
		log.Printf("ERROR: %#v \n", string(temp))
		debug.PrintStack()
		return err
	}
	return
}

func (this WorldMsg) ToModel() (result World, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	result.Owner = this.Owner
	result.mux = &sync.Mutex{}
	for key, room := range this.Rooms {
		roomModel, err := room.ToModel()
		if err != nil {
			return result, err
		}
		if result.Rooms == nil {
			result.Rooms = map[string]*Room{}
		}
		result.Rooms[key] = &roomModel
	}
	return
}

func (this RoomMsg) ToModel() (result Room, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	for key, device := range this.Devices {
		deviceModel, err := device.ToModel()
		if err != nil {
			return result, err
		}
		if result.Devices == nil {
			result.Devices = map[string]*Device{}
		}
		result.Devices[key] = &deviceModel
	}
	return
}

func (this DeviceMsg) ToModel() (result Device, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	for _, service := range this.Services {
		if result.Services == nil {
			result.Services = map[string]Service{}
		}
		result.Services[service.Id] = service
	}
	return
}

func (this World) ToMsg() (result WorldMsg, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	result.Owner = this.Owner
	for key, room := range this.Rooms {
		roomMsg, err := room.ToMsg()
		if err != nil {
			return result, err
		}
		if result.Rooms == nil {
			result.Rooms = map[string]RoomMsg{}
		}
		result.Rooms[key] = roomMsg
	}
	return
}

func (this Room) ToMsg() (result RoomMsg, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	for key, device := range this.Devices {
		deviceMsg, err := device.ToMsg()
		if err != nil {
			return result, err
		}
		if result.Devices == nil {
			result.Devices = map[string]DeviceMsg{}
		}
		result.Devices[key] = deviceMsg
	}
	return
}

func (this Device) ToMsg() (result DeviceMsg, err error) {
	this.States = CleanStates(this.States)
	err = jsonCopy(this, &result)
	for _, service := range this.Services {
		if result.Services == nil {
			result.Services = map[string]Service{}
		}
		result.Services[service.Id] = service
	}
	return
}
