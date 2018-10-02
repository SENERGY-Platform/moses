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
	"github.com/google/uuid"
)

func isAdmin(jwt Jwt) bool {
	for _, role := range jwt.RealmAccess.Roles {
		if role == "admin" {
			return true
		}
	}
	return false
}

func (this *StateRepo) ReadWorlds(jwt Jwt) (worlds []WorldMsg, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	isAdmin := isAdmin(jwt)
	for _, world := range this.Worlds {
		if isAdmin || world.Owner == jwt.UserId {
			msg, err := world.ToMsg()
			if err != nil {
				return worlds, err
			}
			worlds = append(worlds, msg)
		}
	}
	return
}

func (this *StateRepo) CreateWorld(jwt Jwt, msg CreateWorldRequest) (world WorldMsg, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return world, err
	}
	world = WorldMsg{Id: uid.String(), Name: msg.Name, States: msg.States, Owner: jwt.UserId}
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) ReadWorld(jwt Jwt, id string) (world WorldMsg, access bool, exists bool, err error) {
	world, exists, err = this.DevGetWorld(id)
	if err != nil {
		return
	}
	if !isAdmin(jwt) && world.Owner != jwt.UserId {
		return WorldMsg{}, false, exists, err
	}
	return world, true, exists, err
}

func (this *StateRepo) UpdateWorld(jwt Jwt, msg UpdateWorldRequest) (world WorldMsg, access bool, exists bool, err error) {
	world, access, exists, err = this.ReadWorld(jwt, msg.Id)
	if err != nil || !access || !exists {
		world = WorldMsg{}
		return
	}
	world.Name = msg.Name
	world.States = msg.States
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) DeleteWorld(jwt Jwt, id string) (access bool, exists bool, err error) {
	world, exists, err := this.DevGetWorld(id)
	if err != nil {
		return access, exists, err
	}
	if !isAdmin(jwt) && world.Owner != jwt.UserId {
		return false, exists, err
	}
	this.DevDeleteWorld(id)
	return
}

func (this *StateRepo) ReadRoom(jwt Jwt, id string) (room RoomResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	admin := isAdmin(jwt)
	world, exists := this.roomWorldIndex[id]
	if !exists {
		return room, admin, exists, nil
	}
	if !admin && world.Owner != jwt.UserId {
		return room, false, exists, nil
	}
	room.World = world.Id
	room.Room, err = world.Rooms[id].ToMsg()
	return room, access, exists, err
}

func (this *StateRepo) UpdateRoom(jwt Jwt, msg UpdateRoomRequest) (room RoomResponse, access bool, exists bool, err error) {
	room, access, exists, err = this.ReadRoom(jwt, msg.Id)
	if err != nil || !access || !exists {
		return
	}
	room.Room.States = msg.States
	room.Room.Name = msg.Name
	room.Room.Id = msg.Id
	err = this.DevUpdateRoom(room.World, room.Room)
	return
}

func (this *StateRepo) CreateRoom(jwt Jwt, msg CreateRoomRequest) (room RoomResponse, access bool, worldExists bool, err error) {
	worldMsg := WorldMsg{}
	worldMsg, access, worldExists, err = this.ReadWorld(jwt, msg.World)
	if err != nil || !access || !worldExists {
		return room, access, worldExists, err
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return room, true, true, err
	}
	room.Room.Id = uid.String()
	room.Room.Name = msg.Name
	room.Room.States = msg.States
	room.World = worldMsg.Id
	err = this.DevUpdateRoom(room.World, room.Room)
	return
}

func (this *StateRepo) DeleteRoom(jwt Jwt, id string) (room RoomResponse, access bool, exists bool, err error) {
	room, access, exists, err = this.ReadRoom(jwt, id)
	if err != nil || !access || !exists {
		return
	}
	world := WorldMsg{}
	world, exists, err = this.DevGetWorld(room.World)
	if err != nil {
		return
	}
	if !exists {
		err = errors.New("inconsistent world existence read")
		return
	}
	delete(world.Rooms, room.Room.Id)
	err = this.DevUpdateWorld(world)
	return
}
