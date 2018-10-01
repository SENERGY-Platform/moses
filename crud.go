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

func (this *StateRepo) ReadWorlds(jwt Jwt) (worlds []World) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	isAdmin := isAdmin(jwt)
	for _, world := range this.Worlds {
		if isAdmin || world.Owner == jwt.UserId {
			worlds = append(worlds, *world)
		}
	}
	return
}

func (this *StateRepo) CreateWorld(jwt Jwt, msg CreateWorldRequest) (world World, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return world, err
	}
	world = World{Id: uid.String(), Name: msg.Name, States: msg.States, Owner: jwt.UserId}
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) ReadWorld(jwt Jwt, id string) (world World, access bool, exists bool) {
	world, exists = this.DevGetWorld(id)
	if !isAdmin(jwt) && world.Owner != jwt.UserId {
		return World{}, false, exists
	}
	return world, true, exists
}

func (this *StateRepo) UpdateWorld(jwt Jwt, msg UpdateWorldRequest) (world World, access bool, exists bool, err error) {
	world, access, exists = this.ReadWorld(jwt, msg.Id)
	if !access || !exists {
		world = World{}
		return
	}
	world.Name = msg.Name
	world.States = msg.States
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) DeleteWorld(jwt Jwt, id string) (access bool, exists bool) {
	world, exists := this.DevGetWorld(id)
	if !isAdmin(jwt) && world.Owner != jwt.UserId {
		return false, exists
	}
	this.DevDeleteWorld(id)
	return
}

func (this *StateRepo) ReadRoom(jwt Jwt, id string) (room RoomResponse, access bool, exists bool) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	admin := isAdmin(jwt)
	world, exists := this.roomWorldIndex[id]
	if !exists {
		return room, admin, exists
	}
	if !admin && world.Owner != jwt.UserId {
		return room, false, exists
	}
	room.World = world.Id
	room.Room = *(world.Rooms[id])
	return room, access, exists
}

func (this *StateRepo) UpdateRoom(jwt Jwt, msg UpdateRoomRequest) (room RoomResponse, access bool, exists bool, err error) {
	room, access, exists = this.ReadRoom(jwt, msg.Id)
	if !access || !exists {
		return
	}
	world, exists := this.DevGetWorld(room.World)
	if !exists {
		err = errors.New("inconsistent world existence read")
		return
	}
	room.Room.States = msg.States
	room.Room.Name = msg.Name
	room.Room.Id = msg.Id
	world.Rooms[room.Room.Id] = &room.Room
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) CreateRoom(jwt Jwt, msg CreateRoomRequest) (room RoomResponse, access bool, worldExists bool, err error) {
	world, worldExists := this.DevGetWorld(msg.World)
	admin := isAdmin(jwt)
	if !worldExists {
		return room, admin, worldExists, nil
	}
	if !admin && world.Owner != jwt.UserId {
		return room, false, worldExists, nil
	}

	uid, err := uuid.NewRandom()
	if err != nil {
		return room, true, true, err
	}
	room.Room.Id = uid.String()
	room.Room.Name = msg.Name
	room.Room.States = msg.States
	room.World = world.Id
	world.Rooms[room.Room.Id] = &room.Room
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) DeleteRoom(jwt Jwt, id string) (room RoomResponse, access bool, exists bool, err error) {
	room, access, exists = this.ReadRoom(jwt, id)
	if !access || !exists {
		return
	}
	world, exists := this.DevGetWorld(room.World)
	if !exists {
		err = errors.New("inconsistent world existence read")
		return
	}
	delete(world.Rooms, room.Room.Id)
	err = this.DevUpdateWorld(world)
	return
}
