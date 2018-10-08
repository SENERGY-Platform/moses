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
	world.mux.Lock()
	defer world.mux.Unlock()
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

func (this *StateRepo) ReadDevice(jwt Jwt, id string) (device DeviceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	admin := isAdmin(jwt)
	world, exists := this.deviceWorldIndex[id]
	if !exists {
		return device, admin, exists, nil
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	if !admin && world.Owner != jwt.UserId {
		return device, false, exists, nil
	}

	room, exists := this.deviceRoomIndex[id]
	if !exists {
		return device, admin, exists, errors.New("inconsistent deviceRoomIndex")
	}

	device.World = world.Id
	device.Room = room.Id
	device.Device, err = room.Devices[id].ToMsg()
	return device, access, exists, err
}

func (this *StateRepo) CreateDevice(jwt Jwt, msg CreateDeviceRequest) (device DeviceResponse, access bool, worldAndExists bool, err error) {
	room := RoomResponse{}
	room, access, worldAndExists, err = this.ReadRoom(jwt, msg.Room)
	if err != nil || !access || !worldAndExists {
		return device, access, worldAndExists, err
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return device, true, true, err
	}
	device.Device.Id = uid.String()
	device.Device.Name = msg.Name
	device.Device.States = msg.States
	device.Device.ExternalRef = msg.ExternalRef
	device.World = room.World
	device.Room = msg.Room
	err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	return
}

func (this *StateRepo) UpdateDevice(jwt Jwt, msg UpdateDeviceRequest) (device DeviceResponse, access bool, exists bool, err error) {
	device, access, exists, err = this.ReadDevice(jwt, msg.Id)
	if err != nil || !access || !exists {
		return
	}
	device.Device.States = msg.States
	device.Device.Name = msg.Name
	device.Device.Id = msg.Id
	device.Device.ExternalRef = msg.ExternalRef
	err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	return
}

func (this *StateRepo) DeleteDevice(jwt Jwt, id string) (device DeviceResponse, access bool, exists bool, err error) {
	device, access, exists, err = this.ReadDevice(jwt, id)
	if err != nil || !access || !exists {
		return
	}
	world := WorldMsg{}
	world, exists, err = this.DevGetWorld(device.World)
	if err != nil {
		return
	}
	if !exists {
		err = errors.New("inconsistent world existence read")
		return
	}
	delete(world.Rooms[device.Room].Devices, device.Device.Id)
	err = this.DevUpdateWorld(world) //update world is more efficient than update room
	return
}

func (this *StateRepo) ReadService(jwt Jwt, id string) (service ServiceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	admin := isAdmin(jwt)
	devicep, exists := this.serviceDeviceIndex[id]
	if !exists {
		return service, admin, exists, nil
	}
	world, exists := this.deviceWorldIndex[devicep.Id]
	if !exists {
		return service, admin, exists, errors.New("inconsistent deviceWorldIndex")
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	if !admin && world.Owner != jwt.UserId {
		return service, false, exists, nil
	}

	room, exists := this.deviceRoomIndex[devicep.Id]
	if !exists {
		return service, admin, exists, errors.New("inconsistent deviceRoomIndex")
	}

	service.World = world.Id
	service.Room = room.Id
	service.Device = devicep.Id
	serviceModel := devicep.Services[id]
	service.Service.Id = serviceModel.Id
	service.Service.ExternalRef = serviceModel.ExternalRef
	service.Service.Name = serviceModel.Name
	service.Service.Code = serviceModel.Code
	service.Service.SensorInterval = serviceModel.SensorInterval
	return service, access, exists, err
}

func (this *StateRepo) CreateService(jwt Jwt, msg CreateServiceRequest) (service ServiceResponse, access bool, worldAndExists bool, err error) {
	device := DeviceResponse{}
	device, access, worldAndExists, err = this.ReadDevice(jwt, msg.Device)
	if err != nil || !access || !worldAndExists {
		return service, access, worldAndExists, err
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return service, true, true, err
	}
	service.Service.Id = uid.String()
	service.Service.Name = msg.Name
	service.Service.ExternalRef = msg.ExternalRef
	service.Service.Code = msg.Code
	service.Service.SensorInterval = msg.SensorInterval
	service.World = device.World
	service.Room = device.Room
	service.Device = device.Device.Id
	device.Device.Services[service.Service.Id], err = this.PopulateServiceService(jwt, service.Service)
	if err != nil {
		return service, access, worldAndExists, err
	}
	err = this.DevUpdateDevice(service.World, service.Room, device.Device)
	return service, access, worldAndExists, err
}

func (this *StateRepo) PopulateServiceService(jwt Jwt, serviceMsg UpdateServiceRequest) (service Service, err error) {
	service.Id = serviceMsg.Id
	service.Name = serviceMsg.Name
	service.SensorInterval = serviceMsg.SensorInterval
	service.Code = serviceMsg.Code
	service.ExternalRef = serviceMsg.ExternalRef
	service.Marshaller.Service, err = this.GetIotService(jwt, service.ExternalRef)
	return
}

func (this *StateRepo) UpdateService(jwt Jwt, msg UpdateServiceRequest) (service ServiceResponse, access bool, exists bool, err error) {
	service, access, exists, err = this.ReadService(jwt, msg.Id)
	if err != nil || !access || !exists {
		return
	}
	device, access, exists, err := this.ReadDevice(jwt, service.Device)
	if err != nil || !access || !exists {
		return service, access, exists, err
	}
	service.Service.Name = msg.Name
	service.Service.ExternalRef = msg.ExternalRef
	service.Service.Code = msg.Code
	service.Service.SensorInterval = msg.SensorInterval
	device.Device.Services[service.Service.Id], err = this.PopulateServiceService(jwt, service.Service)
	if err != nil {
		return service, access, exists, err
	}
	err = this.DevUpdateDevice(service.World, service.Room, device.Device)
	return service, access, exists, err
}

func (this *StateRepo) DeleteService(jwt Jwt, id string) (service ServiceResponse, access bool, exists bool, err error) {
	service, access, exists, err = this.ReadService(jwt, id)
	if err != nil || !access || !exists {
		return
	}
	device, access, exists, err := this.ReadDevice(jwt, service.Device)
	if err != nil || !access || !exists {
		return service, access, exists, err
	}
	delete(device.Device.Services, service.Service.Id)
	err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	return
}
