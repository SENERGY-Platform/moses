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
	"errors"
	"log"

	"github.com/google/uuid"
)

//Update for HTTP-API
//Stops all change routines and redeploys new world
//requests a mutex lock on the state repo
func (this *StateRepo) UpdateWorld(world World) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	if world.Id == "" {
		uuid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		world.Id = uuid.String()
	}
	err = this.Stop()
	if err != nil {
		return err
	}
	this.Worlds[world.Id] = &world
	this.Start()
	return this.PersistWorld(world)
}

//Update for HTTP-API
//Stops all change routines and redeploys new world with new room
//requests a mutex lock on the state repo
func (this *StateRepo) UpdateRoom(worldId string, room Room) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if worldId == "" {
		return errors.New("missing world id")
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	world, ok := this.Worlds[worldId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
	}
	if world.Rooms == nil {
		world.Rooms = map[string]*Room{}
	}
	if room.Id == "" {
		uuid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		room.Id = uuid.String()
	}
	err = this.Stop()
	if err != nil {
		return err
	}
	world.Rooms[room.Id] = &room
	this.Worlds[world.Id] = world
	this.Start()
	return this.PersistWorld(*world)
}

//Update for HTTP-API
//Stops all change routines and redeploys new world with new room and device
//requests a mutex lock on the state repo
func (this *StateRepo) UpdateDevice(worldId string, roomId string, device Device) (err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	if worldId == "" {
		return errors.New("missing world id")
	}
	if this.Worlds == nil {
		this.Worlds = map[string]*World{}
	}
	world, ok := this.Worlds[worldId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
	}
	if world.Rooms == nil {
		world.Rooms = map[string]*Room{}
	}
	if roomId == "" {
		return errors.New("missing room id")
	}
	if this.Worlds[worldId].Rooms == nil {
		this.Worlds[worldId].Rooms = map[string]*Room{}
	}
	room, ok := this.Worlds[worldId].Rooms[roomId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
	}
	if room.Devices == nil {
		room.Devices = map[string]*Device{}
	}
	if device.Id == "" {
		uuid, err := uuid.NewRandom()
		if err != nil {
			log.Println("ERROR: ", err)
			return err
		}
		device.Id = uuid.String()
	}
	err = this.Stop()
	if err != nil {
		return err
	}
	room.Devices[device.Id] = &device
	world.Rooms[room.Id] = room
	this.Worlds[world.Id] = world
	this.Start()
	return this.PersistWorld(*world)
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
	this.deviceIndex = nil
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
	this.deviceIndex = map[string]*Device{}
	for _, world := range this.Worlds {
		tickers, stops, err := this.StartWorld(world)
		if err != nil {
			panic(err)
		}
		this.changeRoutinesTickers = append(this.changeRoutinesTickers, tickers...)
		this.stopChannels = append(this.stopChannels, stops...)
	}
	return
}

//persists given world; will not stop any change routines, nor will it request a lock on the world mutex
func (this *StateRepo) PersistWorld(world World) (err error) {
	return this.Persistence.PersistWorld(world)
}

func (this *StateRepo) SendSensorData(device *Device, service Service, value interface{}) {
	err := this.Protocol.Send(device.Id, service.Id, value)
	if err != nil {
		log.Println("ERROR: while sending sensor data", value, device.Id, service.Id, err)
	}
}
