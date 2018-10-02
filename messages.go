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
	"time"
)

type CreateWorldRequest struct {
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type UpdateWorldRequest struct {
	Id     string                 `json:"id"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
}

type RoomResponse struct {
	World string  `json:"world"`
	Room  RoomMsg `json:"room"`
}

type UpdateRoomRequest struct {
	Id     string                 `json:"id"`
	Name   string                 `json:"name"`
	States map[string]interface{} `json:"states"`
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
	Id          string                 `json:"id"`
	Name        string                 `json:"name"`
	States      map[string]interface{} `json:"states"`
	ExternalRef string                 `json:"external_ref"` //platform intern device id; 1:1
}

type CreateDeviceRequest struct {
	Room        string                 `json:"room"`
	Name        string                 `json:"name"`
	States      map[string]interface{} `json:"states"`
	ExternalRef string                 `json:"external_ref"` //platform intern device id; 1:1
}

type UpdateServiceRequest struct {
	Id             string        `json:"id"`
	Name           string        `json:"name"`
	ExternalRef    string        `json:"external_ref"` //platform intern service id
	SensorInterval time.Duration `json:"sensor_interval"`
	Code           string        `json:"code"`
}

type ServiceResponse struct {
	World   string               `json:"world"`
	Room    string               `json:"room"`
	Device  string               `json:"device"`
	Service UpdateServiceRequest `json:"service"`
}

type CreateServiceRequest struct {
	Device         string        `json:"device"`
	Name           string        `json:"name"`
	ExternalRef    string        `json:"external_ref"` //platform intern service id, will be used to populate Service.Marshaller and as endpoint for the connector
	SensorInterval time.Duration `json:"sensor_interval"`
	Code           string        `json:"code"`
}

// msg variants of model without pointers for thread safety

type WorldMsg struct {
	Id             string                 `json:"id"`
	Owner          string                 `json:"-"`
	Name           string                 `json:"name"`
	States         map[string]interface{} `json:"states"`
	Rooms          map[string]RoomMsg     `json:"rooms"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines"`
}

type RoomMsg struct {
	Id             string                 `json:"id"`
	Name           string                 `json:"name"`
	States         map[string]interface{} `json:"states"`
	Devices        map[string]DeviceMsg   `json:"devices"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines"`
}

type DeviceMsg struct {
	Id             string                 `json:"id"`
	Name           string                 `json:"name"`
	ExternalRef    string                 `json:"external_ref"` //platform intern device id; 1:1
	States         map[string]interface{} `json:"states"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines"`
	Services       map[string]Service     `json:"services"`
}

func jsonCopy(from interface{}, to interface{}) (err error) {
	temp, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(temp, to)
}

func (this WorldMsg) ToModel() (result World, err error) {
	err = jsonCopy(this, &result)
	return
}

func (this RoomMsg) ToModel() (result Room, err error) {
	err = jsonCopy(this, &result)
	return
}

func (this DeviceMsg) ToModel() (result Device, err error) {
	err = jsonCopy(this, &result)
	return
}

func (this World) ToMsg() (result WorldMsg, err error) {
	err = jsonCopy(this, &result)
	return
}

func (this Room) ToMsg() (result RoomMsg, err error) {
	err = jsonCopy(this, &result)
	return
}

func (this Device) ToMsg() (result DeviceMsg, err error) {
	err = jsonCopy(this, &result)
	return
}
