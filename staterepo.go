/*
 * Copyright 2018 InfAI (CC SES)
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
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

type StateRepo struct {
	Worlds                 map[string]*World
	Graphs                 map[string]*Graph
	Persistence            PersistenceInterface
	Protocol               ProtocolInterface
	Config                 Config
	changeRoutineIndex     map[string]ChangeRoutineIndexElement
	externalRefDeviceIndex map[string]*Device
	serviceDeviceIndex     map[string]*Device
	deviceRoomIndex        map[string]*Room
	deviceWorldIndex       map[string]*World
	roomWorldIndex         map[string]*World
	changeRoutinesTickers  []*time.Ticker
	stopChannels           []chan bool
	mux                    sync.RWMutex
}

//Update for HTTP-DEV-API
//Stops all change routines and redeploys new world
//requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateWorld(worldMsg WorldMsg) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, err := worldMsg.ToModel()
	if err != nil {
		log.Println("ERROR: ", err)
		return err
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	if world.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		world.Id = uid.String()
	}
	err = this.persistWorld(world)
	if err != nil {
		log.Println("ERROR: ", err)
		return err
	}
	err = this.Stop()
	if err != nil {
		log.Println("ERROR: ", err)
		return err
	}
	this.Worlds[world.Id] = &world
	this.Start()
	return
}

func (this *StateRepo) DevGetWorld(id string) (world WorldMsg, exist bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	worldp, exist := this.Worlds[id]
	if !exist {
		return world, exist, nil
	}
	worldp.mux.Lock()
	defer worldp.mux.Unlock()
	world, err = worldp.ToMsg()
	return world, exist, err
}

func (this *StateRepo) DevDeleteWorld(id string) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	err = this.Persistence.DeleteWorld(id)
	if err != nil {
		return err
	}
	err = this.Stop()
	if err != nil {
		log.Println("ERROR: ", err)
		return err
	}
	delete(this.Worlds, id)
	this.Start()
	return
}

//Update for HTTP-DEV-API
//Stops all change routines and redeploys new world with new room
//requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateRoom(worldId string, room RoomMsg) (err error) {
	if err != nil {
		log.Println("ERROR: ", err)
		return err
	}
	if worldId == "" {
		return errors.New("missing world id")
	}
	world, exists, err := this.DevGetWorld(worldId)
	if !exists {
		return errors.New("unknown world id")
	}
	if world.Rooms == nil {
		world.Rooms = map[string]RoomMsg{}
	}
	if room.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		room.Id = uid.String()
	}
	world.Rooms[room.Id] = room
	worldModel, err := world.ToModel()
	if err != nil {
		return err
	}
	err = this.persistWorld(worldModel)
	if err != nil {
		return err
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	err = this.Stop()
	if err != nil {
		return err
	}
	this.Worlds[world.Id] = &worldModel
	this.Start()
	return
}

//Update for HTTP-DEV-API
//Stops all change routines and redeploys new world with new room and device
//requests a mutex lock on the state repo
func (this *StateRepo) DevUpdateDevice(worldId string, roomId string, device DeviceMsg) (err error) {
	if worldId == "" {
		return errors.New("missing world id")
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	world, exists, err := this.DevGetWorld(worldId)
	if !exists {
		return errors.New("unknown world id")
	}
	if world.Rooms == nil {
		world.Rooms = map[string]RoomMsg{}
	}
	if roomId == "" {
		return errors.New("missing room id")
	}
	room, ok := world.Rooms[roomId]
	if !ok {
		return errors.New("unknown room id: " + roomId)
	}
	if room.Devices == nil {
		room.Devices = map[string]DeviceMsg{}
	}
	if device.Id == "" {
		uid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		device.Id = uid.String()
	}
	room.Devices[device.Id] = device
	world.Rooms[room.Id] = room
	worldModel, err := world.ToModel()
	if err != nil {
		return err
	}

	err = this.persistWorld(worldModel)
	if err != nil {
		return err
	}
	this.mux.Lock()
	defer this.mux.Unlock()
	err = this.Stop()
	if err != nil {
		return err
	}
	this.Worlds[world.Id] = &worldModel
	this.Start()
	return
}

//Stops all change routines if any are running and loads state repo from the database (no restart of change routines)
func (this *StateRepo) Load() (err error) {
	err = this.Stop()
	if err != nil {
		return err
	}
	this.Worlds, err = this.Persistence.LoadWorlds()
	if err != nil {
		return err
	}
	this.Graphs, err = this.Persistence.LoadGraphs()
	return err
}

//stops all change routines; may be called repeatedly while already stopped ore not started
func (this *StateRepo) Stop() (err error) {
	for _, ticker := range this.changeRoutinesTickers {
		ticker.Stop()
	}
	for _, stop := range this.stopChannels {
		stop <- true
	}
	this.stopChannels = nil
	this.changeRoutinesTickers = nil
	this.changeRoutineIndex = nil
	this.externalRefDeviceIndex = nil
	this.serviceDeviceIndex = nil
	this.deviceRoomIndex = nil
	this.deviceWorldIndex = nil
	this.roomWorldIndex = nil
	return
}

//starts change routines; will first call stop() to prevent overpopulation of change routines
//if error occurs, the state repo may be in a partially running state which can not be stopped with Stop()
//in this case a panic occurs
func (this *StateRepo) Start() {
	err := this.Stop()
	if err != nil {
		panic(err)
	}
	this.changeRoutineIndex = map[string]ChangeRoutineIndexElement{}
	this.externalRefDeviceIndex = map[string]*Device{}
	this.serviceDeviceIndex = map[string]*Device{}
	this.deviceRoomIndex = map[string]*Room{}
	this.deviceWorldIndex = map[string]*World{}
	this.roomWorldIndex = map[string]*World{}
	for _, world := range this.Worlds {
		tickers, stops, err := this.StartWorld(world)
		if err != nil {
			panic(err)
		}
		this.changeRoutinesTickers = append(this.changeRoutinesTickers, tickers...)
		this.stopChannels = append(this.stopChannels, stops...)
	}
	if this.Protocol != nil {
		this.Protocol.SetReceiver(func(deviceId string, serviceId string, cmdMsg interface{}, responder func(respMsg interface{})) {
			this.HandleCommand(deviceId, serviceId, cmdMsg, responder)
		})
	} else {
		log.Println("WARNING: no protocol handler set")
	}
	return
}

//persists given world; will not stop any change routines, nor will it request a lock on the world mutex
func (this *StateRepo) persistWorld(world World) (err error) {
	return this.Persistence.PersistWorld(world)
}

func (this *StateRepo) sendSensorData(device *Device, service Service, value interface{}) {
	log.Println("DEBUG: send data to connector", device.Id, device.ExternalRef, service.Id, service.ExternalRef, value)
	if this.Protocol == nil {
		log.Println("WARNING: not protocol connected")
		return
	}
	if device.ExternalRef == "" {
		log.Println("WARNING: no external ref for device")
		return
	}
	if service.ExternalRef == "" {
		log.Println("WARNING: no external ref for service")
		return
	}
	err := this.Protocol.Send(device.ExternalRef, service.ExternalRef, service.Marshaller, value)
	if err != nil {
		log.Println("ERROR: while sending sensor data", value, device.ExternalRef, service.ExternalRef, err)
	}
}

func (this *StateRepo) HandleCommand(externalDeviceRef string, externalServiceRef string, cmdMsg interface{}, responder func(respMsg interface{})) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	device, ok := this.externalRefDeviceIndex[externalDeviceRef]
	if !ok {
		log.Println("WARNING: no device with ref found ", externalDeviceRef)
		return
	}
	world, ok := this.deviceWorldIndex[device.Id]
	if !ok {
		log.Println("WARNING: no world for device found ", device.Id, " ", externalDeviceRef)
		return
	}
	room, ok := this.deviceRoomIndex[device.Id]
	if !ok {
		log.Println("WARNING: no room for device found ", device.Id, " ", externalDeviceRef)
		return
	}

	for _, service := range device.Services {
		if service.ExternalRef == externalServiceRef {
			err := run(service.Code, this.getJsCommandApi(world, room, device, cmdMsg, responder), this.Config.JsTimeout, &world.mux)
			if err != nil {
				log.Println("ERROR: while handling command in jsvm", err)
			}
			return
		}
	}
	log.Println("WARNING: no matching service for device found ", externalServiceRef)
}

func (this *StateRepo) RunService(serviceId string, cmdMsg interface{}) (resp interface{}, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	device, ok := this.serviceDeviceIndex[serviceId]
	if !ok {
		log.Println("WARNING: no device with ref found ", serviceId)
		return
	}

	service, ok := device.Services[serviceId]
	if !ok {
		log.Println("WARNING: no service with id found ", serviceId)
		return
	}

	world, ok := this.deviceWorldIndex[device.Id]
	if !ok {
		log.Println("WARNING: no world for device found ", device.Id, " ", serviceId)
		return
	}
	room, ok := this.deviceRoomIndex[device.Id]
	if !ok {
		log.Println("WARNING: no room for device found ", device.Id, " ", serviceId)
		return
	}
	err = run(service.Code, this.getJsCommandApi(world, room, device, cmdMsg, func(respMsg interface{}) {
		resp = respMsg
	}), this.Config.JsTimeout, &world.mux)
	return
}
