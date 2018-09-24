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

func (this *StateRepo) UpdateWorld(world World) (err error) {
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
	this.Worlds[world.Id] = world
	err = this.Start()
	if err != nil {
		log.Fatal("unable to restart state repo", err)
	}
	return this.PersistWorld(world)
}

func (this *StateRepo) UpdateRoom(worldId string, room Room) (err error) {
	if worldId == "" {
		return errors.New("missing world id")
	}
	world, ok := this.Worlds[worldId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
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
	world.Rooms[room.Id] = room
	this.Worlds[world.Id] = world
	err = this.Start()
	if err != nil {
		log.Fatal("unable to restart state repo", err)
	}
	return this.PersistWorld(world)
}

func (this *StateRepo) UpdateDevice(worldId string, roomId string, device Device) (err error) {
	if worldId == "" {
		return errors.New("missing world id")
	}
	world, ok := this.Worlds[worldId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
	}
	if roomId == "" {
		return errors.New("missing room id")
	}
	room, ok := this.Worlds[worldId].Rooms[roomId]
	if !ok {
		return errors.New("unknown world id: " + worldId)
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
	room.Devices[device.Id] = device
	world.Rooms[room.Id] = room
	this.Worlds[world.Id] = world
	err = this.Start()
	if err != nil {
		log.Fatal("unable to restart state repo", err)
	}
	return this.PersistWorld(world)
}

func (this *StateRepo) Load() (err error) {
	this.Worlds, err = this.Persistence.LoadWorlds()
	this.Graphs, err = this.Persistence.LoadGraphs()
	return
}

func (this *StateRepo) Stop() (err error) {
	log.Println("ERROR: staterepo.stop() not implemented")
	return
}

func (this *StateRepo) Start() (err error) {
	log.Println("ERROR: staterepo.start() not implemented")
	return
}

func (this *StateRepo) PersistWorld(world World) (err error) {
	return this.Persistence.PersistWorld(world)
}
