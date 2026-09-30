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
	"context"
	"encoding/json"
	"errors"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/cbroglie/mustache"
	"github.com/google/uuid"
	"strings"
)

func (this *StateRepo) ReadWorlds(token sc_jwt.Token) (worlds []WorldMsg, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	for _, world := range this.Worlds {
		if world.Owner == token.GetUserId() {
			msg, err := snapshotLocked(world)
			if err != nil {
				return worlds, err
			}
			worlds = append(worlds, msg)
		}
	}
	return
}

func (this *StateRepo) CreateWorld(token sc_jwt.Token, msg CreateWorldRequest) (world WorldMsg, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return world, err
	}
	world = WorldMsg{Id: uid.String(), Name: msg.Name, States: getDefaultWorldStates(msg.States), Owner: token.GetUserId(), ChangeRoutines: getDefaultWorldChangeRoutines()}
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) ReadWorld(token sc_jwt.Token, id string) (world WorldMsg, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, exists, err = this.devGetWorldLocked(id)
	if err != nil || !exists {
		return
	}
	if world.Owner != token.GetUserId() {
		return WorldMsg{}, false, exists, err
	}
	return world, true, true, err
}

// beginWorldEditLocked is ReadWorld for a mutator, with the same results; the caller holds this.mux and calls Start when stopped is true.
// A world that does not convert is an error before it is a denial, as in ReadWorld.
func (this *StateRepo) beginWorldEditLocked(token sc_jwt.Token, id string) (world WorldMsg, access bool, exists bool, stopped bool, err error) {
	worldp, exists := this.Worlds[id]
	if !exists {
		return world, false, false, false, nil
	}
	if worldp.Owner != token.GetUserId() {
		_, err = snapshotLocked(worldp)
		return WorldMsg{}, false, true, false, err
	}
	world, stopped, err = this.beginEditLocked(worldp)
	if err != nil && stopped {
		return world, false, true, stopped, err
	}
	return world, true, true, stopped, err
}

func (this *StateRepo) UpdateWorld(token sc_jwt.Token, msg UpdateWorldRequest) (world WorldMsg, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, access, exists, stopped, err := this.beginWorldEditLocked(token, msg.Id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return WorldMsg{}, access, exists, err
	}
	world.Name = msg.Name
	world.States = msg.States
	world.ChangeRoutines = msg.ChangeRoutines
	err = this.storeEditLocked(world)
	return world, true, true, err
}

func (this *StateRepo) DeleteWorld(token sc_jwt.Token, id string) (access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	_, access, exists, stopped, err := this.beginWorldEditLocked(token, id)
	if stopped {
		defer this.Start()
	}
	if !exists || (err != nil && !access) {
		util.Logger.Warn("world not found", attributes.ErrorKey, err, "id", id, "exists", exists)
		return access, exists, err
	}
	if !access {
		util.Logger.Warn("access denied", "owner", this.Worlds[id].Owner, "user_id", token.GetUserId())
		return false, exists, err
	}
	if err != nil {
		return true, exists, err
	}
	err = this.deleteWorldLocked(id)
	return true, exists, err
}

// storeEditLocked converts and stores an edited world snapshot; the caller holds this.mux and has stopped the routines.
func (this *StateRepo) storeEditLocked(world WorldMsg) error {
	model, err := world.ToModel()
	if err != nil {
		util.Logger.Error("unable to convert world message to model", attributes.ErrorKey, err)
		return err
	}
	return this.storeWorldLocked(model)
}

// worldOfRoomLocked is the lookup and owner check of ReadRoom; the caller holds this.mux.
func (this *StateRepo) worldOfRoomLocked(token sc_jwt.Token, id string) (world *World, access bool, exists bool) {
	world, exists = this.roomWorldIndex[id]
	if !exists {
		util.Logger.Warn("no world for room id found", "id", id)
		return nil, false, exists
	}
	if world.Owner != token.GetUserId() {
		util.Logger.Warn("access denied", "owner", world.Owner, "user_id", token.GetUserId())
		return world, false, exists
	}
	return world, true, true
}

func (this *StateRepo) ReadRoom(token sc_jwt.Token, id string) (room RoomResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	world, access, exists := this.worldOfRoomLocked(token, id)
	if !access || !exists {
		return room, access, exists, nil
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	room.World = world.Id
	room.Room, err = world.Rooms[id].ToMsg()
	return room, true, true, err
}

// beginRoomEditLocked is ReadRoom for a mutator, returning the whole world snapshot the room is taken from;
// the caller holds this.mux and calls Start when stopped is true.
func (this *StateRepo) beginRoomEditLocked(token sc_jwt.Token, id string) (world WorldMsg, room RoomResponse, access bool, exists bool, stopped bool, err error) {
	worldp, access, exists := this.worldOfRoomLocked(token, id)
	if !access || !exists {
		return world, room, access, exists, false, nil
	}
	room.World = worldp.Id
	world, stopped, err = this.beginEditLocked(worldp)
	if err != nil {
		return world, room, true, true, stopped, err
	}
	room.Room = world.Rooms[id]
	return world, room, true, true, stopped, nil
}

func (this *StateRepo) UpdateRoom(token sc_jwt.Token, msg UpdateRoomRequest) (room RoomResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, room, access, exists, stopped, err := this.beginRoomEditLocked(token, msg.Id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		util.Logger.Warn("unable to update room", attributes.ErrorKey, err, "id", msg.Id, "access", access, "exists", exists)
		return
	}
	room.Room.States = msg.States
	room.Room.Name = msg.Name
	room.Room.Id = msg.Id
	room.Room.ChangeRoutines = msg.ChangeRoutines
	model, err := withRoom(world, room.Room)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return
}

func (this *StateRepo) CreateRoom(token sc_jwt.Token, msg CreateRoomRequest) (room RoomResponse, access bool, worldExists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	worldMsg, access, worldExists, stopped, err := this.beginWorldEditLocked(token, msg.World)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !worldExists {
		return room, access, worldExists, err
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return room, true, true, err
	}
	room.Room.Id = uid.String()
	room.Room.Name = msg.Name
	room.Room.States = getDefaultRoomStates(msg.States)
	room.World = worldMsg.Id
	room.Room.ChangeRoutines, err = getDefaultRoomChangeRoutines()
	if err != nil {
		return room, true, true, err
	}
	model, err := withRoom(worldMsg, room.Room)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return room, true, true, err
}

func (this *StateRepo) DeleteRoom(token sc_jwt.Token, id string) (room RoomResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, room, access, exists, stopped, err := this.beginRoomEditLocked(token, id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return
	}
	delete(world.Rooms, room.Room.Id)
	err = this.storeEditLocked(world)
	return room, true, true, err
}

// worldOfDeviceLocked is the lookup and owner check of ReadDevice; the caller holds this.mux.
func (this *StateRepo) worldOfDeviceLocked(token sc_jwt.Token, id string) (world *World, room *Room, access bool, exists bool, err error) {
	world, exists = this.deviceWorldIndex[id]
	if !exists {
		return nil, nil, false, exists, nil
	}
	if world.Owner != token.GetUserId() {
		return world, nil, false, exists, nil
	}
	room, exists = this.deviceRoomIndex[id]
	if !exists {
		return world, nil, false, exists, errors.New("inconsistent deviceRoomIndex")
	}
	return world, room, true, true, nil
}

func (this *StateRepo) ReadDevice(token sc_jwt.Token, id string) (device DeviceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	world, room, access, exists, err := this.worldOfDeviceLocked(token, id)
	if err != nil || !access || !exists {
		return device, access, exists, err
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	device.World = world.Id
	device.Room = room.Id
	device.Device, err = room.Devices[id].ToMsg()
	return device, true, true, err
}

// beginDeviceEditLocked is ReadDevice for a mutator, returning the whole world snapshot the device is taken from;
// the caller holds this.mux and calls Start when stopped is true.
func (this *StateRepo) beginDeviceEditLocked(token sc_jwt.Token, id string) (world WorldMsg, device DeviceResponse, access bool, exists bool, stopped bool, err error) {
	worldp, room, access, exists, err := this.worldOfDeviceLocked(token, id)
	if err != nil || !access || !exists {
		return world, device, access, exists, false, err
	}
	device.World = worldp.Id
	device.Room = room.Id
	world, stopped, err = this.beginEditLocked(worldp)
	if err != nil {
		return world, device, true, true, stopped, err
	}
	device.Device = world.Rooms[room.Id].Devices[id]
	return world, device, true, true, stopped, nil
}

func (this *StateRepo) CreateDevice(token sc_jwt.Token, msg CreateDeviceRequest) (device DeviceResponse, access bool, worldAndExists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, room, access, worldAndExists, stopped, err := this.beginRoomEditLocked(token, msg.Room)
	if stopped {
		defer this.Start()
	}
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
	device.Device.ChangeRoutines = map[string]ChangeRoutine{}
	model, err := withDevice(world, device.Room, device.Device)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return device, true, true, err
}

func (this *StateRepo) UpdateDevice(token sc_jwt.Token, msg UpdateDeviceRequest) (device DeviceResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, device, access, exists, stopped, err := this.beginDeviceEditLocked(token, msg.Id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return
	}
	device.Device.States = msg.States
	device.Device.Name = msg.Name
	device.Device.Id = msg.Id
	device.Device.ExternalRef = msg.ExternalRef
	device.Device.ChangeRoutines = msg.ChangeRoutines
	device.Device.Services = msg.Services
	for key, value := range msg.Services {
		device.Device.Services[key], err = this.PopulateServiceService(token, UpdateServiceRequest{Id: value.Id, Code: value.Code, SensorInterval: value.SensorInterval, ExternalRef: value.ExternalRef, Name: value.Name})
		if err != nil {
			util.Logger.Warn("unable to populate service", attributes.ErrorKey, err, "service_id", value.Id)
			return device, true, true, err
		}
	}
	model, err := withDevice(world, device.Room, device.Device)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return device, true, true, err
}

func (this *StateRepo) DeleteDevice(ctx context.Context, token sc_jwt.Token, id string) (device DeviceResponse, access bool, exists bool, err error) {
	device, access, exists, err = this.removeDevice(token, id)
	//outside the lock, so the device-manager call holds up no api change or command; only a removed device gets here without error
	if err == nil && access && exists {
		if externalErr := this.DeleteExternalDevice(ctx, token, device.Device.ExternalRef); externalErr != nil {
			util.Logger.Warn("unable to delete the platform device of a deleted device", attributes.ErrorKey, externalErr, "external_ref", device.Device.ExternalRef)
		}
	}
	return device, access, exists, err
}

// removeDevice is the world change of DeleteDevice.
func (this *StateRepo) removeDevice(token sc_jwt.Token, id string) (device DeviceResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, device, access, exists, stopped, err := this.beginDeviceEditLocked(token, id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return
	}
	delete(world.Rooms[device.Room].Devices, device.Device.Id)
	err = this.storeEditLocked(world)
	return device, true, true, err
}

// worldOfServiceLocked is the lookup and owner check of ReadService; the caller holds this.mux.
func (this *StateRepo) worldOfServiceLocked(token sc_jwt.Token, id string) (world *World, room *Room, device *Device, access bool, exists bool, err error) {
	device, exists = this.serviceDeviceIndex[id]
	if !exists {
		return nil, nil, nil, false, exists, nil
	}
	world, exists = this.deviceWorldIndex[device.Id]
	if !exists {
		return nil, nil, device, false, exists, errors.New("inconsistent deviceWorldIndex")
	}
	if world.Owner != token.GetUserId() {
		return world, nil, device, false, exists, nil
	}
	room, exists = this.deviceRoomIndex[device.Id]
	if !exists {
		return world, nil, device, false, exists, errors.New("inconsistent deviceRoomIndex")
	}
	return world, room, device, true, true, nil
}

// serviceResponse is what ReadService returns for service id of device in room of world.
func serviceResponse(world *World, room *Room, device *Device, id string) (service ServiceResponse) {
	service.World = world.Id
	service.Room = room.Id
	service.Device = device.Id
	serviceModel := device.Services[id]
	service.Service.Id = serviceModel.Id
	service.Service.ExternalRef = serviceModel.ExternalRef
	service.Service.Name = serviceModel.Name
	service.Service.Code = serviceModel.Code
	service.Service.SensorInterval = serviceModel.SensorInterval
	return service
}

func (this *StateRepo) ReadService(token sc_jwt.Token, id string) (service ServiceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	world, room, device, access, exists, err := this.worldOfServiceLocked(token, id)
	if err != nil || !access || !exists {
		return service, access, exists, err
	}
	return serviceResponse(world, room, device, id), true, true, nil
}

// beginServiceEditLocked is ReadService followed by ReadDevice of its device for a mutator, returning the whole
// world snapshot the device is taken from; the caller holds this.mux and calls Start when stopped is true.
func (this *StateRepo) beginServiceEditLocked(token sc_jwt.Token, id string) (world WorldMsg, service ServiceResponse, device DeviceResponse, access bool, exists bool, stopped bool, err error) {
	worldp, room, devicep, access, exists, err := this.worldOfServiceLocked(token, id)
	if err != nil || !access || !exists {
		return world, service, device, access, exists, false, err
	}
	//a service is only ever replaced with its world, so its fields are read like ReadService reads them
	service = serviceResponse(worldp, room, devicep, id)
	device.World = worldp.Id
	device.Room = room.Id
	world, stopped, err = this.beginEditLocked(worldp)
	if err != nil {
		return world, service, device, true, true, stopped, err
	}
	device.Device = world.Rooms[room.Id].Devices[devicep.Id]
	return world, service, device, true, true, stopped, nil
}

func (this *StateRepo) CreateService(token sc_jwt.Token, msg CreateServiceRequest) (service ServiceResponse, access bool, worldAndExists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, device, access, worldAndExists, stopped, err := this.beginDeviceEditLocked(token, msg.Device)
	if stopped {
		defer this.Start()
	}
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
	if device.Device.Services == nil {
		device.Device.Services = map[string]Service{}
	}
	device.Device.Services[service.Service.Id], err = this.PopulateServiceService(token, service.Service)
	if err != nil {
		return service, access, worldAndExists, err
	}
	model, err := withDevice(world, service.Room, device.Device)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return service, true, true, err
}

func (this *StateRepo) PopulateServiceService(token sc_jwt.Token, serviceMsg UpdateServiceRequest) (service Service, err error) {
	service.Id = serviceMsg.Id
	service.Name = serviceMsg.Name
	service.SensorInterval = serviceMsg.SensorInterval
	service.Code = serviceMsg.Code
	service.ExternalRef = serviceMsg.ExternalRef
	return
}

func (this *StateRepo) UpdateService(token sc_jwt.Token, msg UpdateServiceRequest) (service ServiceResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, service, device, access, exists, stopped, err := this.beginServiceEditLocked(token, msg.Id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return service, access, exists, err
	}
	service.Service.Name = msg.Name
	service.Service.ExternalRef = msg.ExternalRef
	service.Service.Code = msg.Code
	service.Service.SensorInterval = msg.SensorInterval
	device.Device.Services[service.Service.Id], err = this.PopulateServiceService(token, service.Service)
	if err != nil {
		return service, access, exists, err
	}
	model, err := withDevice(world, service.Room, device.Device)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return service, true, true, err
}

func (this *StateRepo) DeleteService(token sc_jwt.Token, id string) (service ServiceResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	world, service, device, access, exists, stopped, err := this.beginServiceEditLocked(token, id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return service, access, exists, err
	}
	delete(device.Device.Services, service.Service.Id)
	model, err := withDevice(world, device.Room, device.Device)
	if err == nil {
		err = this.storeWorldLocked(model)
	}
	return service, true, true, err
}

func (this *StateRepo) CreateDeviceByType(ctx context.Context, token sc_jwt.Token, msg CreateDeviceByTypeRequest) (result DeviceResponse, access bool, worldAndExists bool, err error) {
	room, access, worldAndExists, err := this.ReadRoom(token, msg.Room)
	if err != nil || !access || !worldAndExists {
		return result, access, worldAndExists, err
	}
	services, err := this.prepareServices(ctx, token, msg.DeviceTypeId)
	if err != nil {
		return result, access, worldAndExists, err
	}
	externalDevice, err := this.GenerateExternalDevice(ctx, token, msg)
	if err != nil {
		return result, access, worldAndExists, err
	}
	uid, err := uuid.NewRandom()
	if err != nil {
		return result, access, worldAndExists, err
	}
	result.Device.Id = uid.String()
	result.Device.Name = msg.Name
	result.Device.ExternalTypeId = externalDevice.DeviceTypeId
	result.Device.ExternalRef = externalDevice.Id
	result.World = room.World
	result.Room = msg.Room
	result.Device.Services = services
	//the device-manager calls above stay outside the lock; DevUpdateDevice re-reads the world under it
	err = this.DevUpdateDevice(result.World, result.Room, result.Device)
	return result, true, true, err
}

func (this *StateRepo) prepareServices(ctx context.Context, token sc_jwt.Token, deviceTypeId string) (result map[string]Service, err error) {
	result = map[string]Service{}
	devicetype, err := this.GetIotDeviceType(ctx, token, deviceTypeId)
	if err != nil {
		return result, err
	}
	for _, externalService := range devicetype.Services {
		uid, err := uuid.NewRandom()
		if err != nil {
			return result, err
		}
		service := Service{Id: uid.String(), Name: externalService.Name, ExternalRef: externalService.Id}
		service.Code, err = this.createServiceCodeSkeleton(externalService)
		if err != nil {
			return result, err
		}
		result[service.Id] = service
	}
	return result, err
}

func (this *StateRepo) createServiceCodeSkeleton(service model.Service) (result string, err error) {
	templateParamer := map[string]string{}
	if len(service.Outputs) > 0 {
		output := service.Outputs[0]
		formatedOutput, err := inputOutputSkeletonString(output.ContentVariable)
		if err != nil {
			return result, err
		}
		templateParamer["output"] = strings.TrimSpace(formatedOutput)
	}

	if len(service.Inputs) > 0 {
		input := service.Inputs[0]
		formatedInput, err := inputOutputSkeletonString(input.ContentVariable)
		if err != nil {
			return result, err
		}
		templateParamer["input"] = strings.TrimSpace(formatedInput)
	}

	template := `{{#input}}/*
{{{input}}}
*/
var input = moses.service.input; 
{{/input}}{{#output}}var output = {{{output}}};
moses.service.send(output);{{/output}}`
	return mustache.Render(template, templateParamer)
}

func (this *StateRepo) CreateChangeRoutine(token sc_jwt.Token, msg CreateChangeRoutineRequest) (result ChangeRoutineResponse, access bool, exists bool, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return result, access, exists, err
	}
	routine := ChangeRoutine{Interval: msg.Interval, Code: msg.Code, Id: uid.String()}
	result = ChangeRoutineResponse{Id: routine.Id, Code: routine.Code, Interval: routine.Interval, RefId: msg.RefId, RefType: msg.RefType}
	this.mux.Lock()
	defer this.mux.Unlock()
	var stopped bool
	// the cases assign the named results instead of shadowing them, so a failed update reaches the caller
	switch msg.RefType {
	case "world":
		var world WorldMsg
		world, access, exists, stopped, err = this.beginWorldEditLocked(token, msg.RefId)
		if stopped {
			defer this.Start()
		}
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if world.ChangeRoutines == nil {
			world.ChangeRoutines = map[string]ChangeRoutine{}
		}
		world.ChangeRoutines[routine.Id] = routine
		err = this.storeEditLocked(world)
	case "room":
		var world WorldMsg
		var room RoomResponse
		world, room, access, exists, stopped, err = this.beginRoomEditLocked(token, msg.RefId)
		if stopped {
			defer this.Start()
		}
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if room.Room.ChangeRoutines == nil {
			room.Room.ChangeRoutines = map[string]ChangeRoutine{}
		}
		room.Room.ChangeRoutines[routine.Id] = routine
		err = this.storeRoomLocked(world, room.Room)
	case "device":
		var world WorldMsg
		var device DeviceResponse
		world, device, access, exists, stopped, err = this.beginDeviceEditLocked(token, msg.RefId)
		if stopped {
			defer this.Start()
		}
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if device.Device.ChangeRoutines == nil {
			device.Device.ChangeRoutines = map[string]ChangeRoutine{}
		}
		device.Device.ChangeRoutines[routine.Id] = routine
		err = this.storeDeviceLocked(world, device.Room, device.Device)
	default:
		err = errors.New("unknown ref type")
	}
	return result, true, true, err
}

// storeRoomLocked stores world with room put in; the caller holds this.mux and has stopped the routines.
func (this *StateRepo) storeRoomLocked(world WorldMsg, room RoomMsg) error {
	model, err := withRoom(world, room)
	if err != nil {
		return err
	}
	return this.storeWorldLocked(model)
}

// storeDeviceLocked stores world with device put into room roomId; the caller holds this.mux and has stopped the routines.
func (this *StateRepo) storeDeviceLocked(world WorldMsg, roomId string, device DeviceMsg) error {
	model, err := withDevice(world, roomId, device)
	if err != nil {
		return err
	}
	return this.storeWorldLocked(model)
}

// changeRoutineEdit is where in a world snapshot a routine lives, as beginChangeRoutineEditLocked found it.
type changeRoutineEdit struct {
	world  WorldMsg
	room   RoomResponse
	device DeviceResponse
}

// routines returns the routine map of the routine's ref in the snapshot.
func (this *changeRoutineEdit) routines(refType string) map[string]ChangeRoutine {
	switch refType {
	case "world":
		return this.world.ChangeRoutines
	case "room":
		return this.room.Room.ChangeRoutines
	case "device":
		return this.device.Device.ChangeRoutines
	}
	return nil
}

// store writes the snapshot back the way the routine's ref type was always written.
func (this *StateRepo) storeChangeRoutineEditLocked(refType string, edit changeRoutineEdit) error {
	switch refType {
	case "world":
		return this.storeEditLocked(edit.world)
	case "room":
		return this.storeRoomLocked(edit.world, edit.room.Room)
	case "device":
		return this.storeDeviceLocked(edit.world, edit.device.Room, edit.device.Device)
	}
	return errors.New("unknown ref type")
}

// beginChangeRoutineEditLocked is ReadChangeRoutine for a mutator, returning the world snapshot the routine is taken from;
// the caller holds this.mux and calls Start when stopped is true.
func (this *StateRepo) beginChangeRoutineEditLocked(token sc_jwt.Token, id string) (edit changeRoutineEdit, routine ChangeRoutineResponse, access bool, exists bool, stopped bool, err error) {
	index, exists := this.changeRoutineIndex[id]
	if !exists {
		return edit, routine, access, exists, false, err
	}
	routine.RefType = index.RefType
	routine.RefId = index.RefId
	routine.Id = id
	switch routine.RefType {
	case "world":
		edit.world, access, exists, stopped, err = this.beginWorldEditLocked(token, routine.RefId)
	case "room":
		edit.world, edit.room, access, exists, stopped, err = this.beginRoomEditLocked(token, routine.RefId)
	case "device":
		edit.world, edit.device, access, exists, stopped, err = this.beginDeviceEditLocked(token, routine.RefId)
	default:
		return edit, routine, true, true, false, errors.New("unknown ref type")
	}
	if err != nil || !access || !exists {
		return edit, routine, access, exists, stopped, err
	}
	found, ok := edit.routines(routine.RefType)[routine.Id]
	if !ok {
		return edit, routine, access, exists, stopped, errors.New("inconsistent routine id existence")
	}
	routine.Code = found.Code
	routine.Interval = found.Interval
	return edit, routine, true, true, stopped, nil
}

func (this *StateRepo) UpdateChangeRoutine(token sc_jwt.Token, msg UpdateChangeRoutineRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	edit, routine, access, exists, stopped, err := this.beginChangeRoutineEditLocked(token, msg.Id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return routine, access, exists, err
	}
	changeRoutine := ChangeRoutine(msg)
	routine.Code = changeRoutine.Code
	routine.Interval = changeRoutine.Interval
	edit.routines(routine.RefType)[msg.Id] = changeRoutine
	err = this.storeChangeRoutineEditLocked(routine.RefType, edit)
	return routine, true, true, err
}

func (this *StateRepo) getChangeRoutineFromIndex(id string) (routine ChangeRoutineIndexElement, exists bool) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	routine, exists = this.changeRoutineIndex[id]
	return
}

func (this *StateRepo) ReadChangeRoutine(token sc_jwt.Token, id string) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	index, exists := this.getChangeRoutineFromIndex(id)
	if !exists {
		return routine, access, exists, err
	}
	routine.RefType = index.RefType
	routine.RefId = index.RefId
	routine.Id = id
	switch routine.RefType {
	case "world":
		var world WorldMsg
		world, access, exists, err = this.ReadWorld(token, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		worldRoutine, ok := world.ChangeRoutines[routine.Id]
		if !ok {
			return routine, access, exists, errors.New("inconsistent routine id existence")
		}
		routine.Code = worldRoutine.Code
		routine.Interval = worldRoutine.Interval
	case "room":
		var room RoomResponse
		room, access, exists, err = this.ReadRoom(token, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		roomRoutine, ok := room.Room.ChangeRoutines[routine.Id]
		if !ok {
			return routine, access, exists, errors.New("inconsistent routine id existence")
		}
		routine.Code = roomRoutine.Code
		routine.Interval = roomRoutine.Interval
	case "device":
		var device DeviceResponse
		device, access, exists, err = this.ReadDevice(token, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		deviceRoutine, ok := device.Device.ChangeRoutines[routine.Id]
		if !ok {
			return routine, access, exists, errors.New("inconsistent routine id existence")
		}
		routine.Code = deviceRoutine.Code
		routine.Interval = deviceRoutine.Interval
	default:
		err = errors.New("unknown ref type")
	}
	return routine, true, true, err
}

func (this *StateRepo) DeleteChangeRoutine(token sc_jwt.Token, id string) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	edit, routine, access, exists, stopped, err := this.beginChangeRoutineEditLocked(token, id)
	if stopped {
		defer this.Start()
	}
	if err != nil || !access || !exists {
		return
	}
	// storing stops every routine and restarts them from this.Worlds, which drops the deleted one's ticker and index entry
	delete(edit.routines(routine.RefType), routine.Id)
	err = this.storeChangeRoutineEditLocked(routine.RefType, edit)
	return routine, true, true, err
}

func (this *StateRepo) CreateTemplate(token sc_jwt.Token, request CreateTemplateRequest) (result RoutineTemplate, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return result, err
	}
	result = RoutineTemplate{Id: uid.String(), Name: request.Name, Description: request.Description, Template: request.Template}
	result.Parameter, err = GetTemplateParameterList(request.Template)
	if err != nil {
		return result, err
	}
	err = this.Persistence.PersistTemplate(result)
	return result, err
}

func (this *StateRepo) UpdateTemplate(token sc_jwt.Token, request UpdateTemplateRequest) (result RoutineTemplate, exists bool, err error) {
	result, err = this.Persistence.GetTemplate(request.Id)
	if errors.Is(err, ErrNotFound) {
		util.Logger.Warn("template not found", "id", request.Id)
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	result = RoutineTemplate{Id: request.Id, Name: request.Name, Description: request.Description, Template: request.Template}
	result.Parameter, err = GetTemplateParameterList(request.Template)
	if err != nil {
		return result, true, err
	}
	err = this.Persistence.PersistTemplate(result)
	return result, true, err
}

func (this *StateRepo) ReadTemplate(token sc_jwt.Token, id string) (result RoutineTemplate, exists bool, err error) {
	result, ok := defaultTemplates[id]
	if ok {
		return result, true, nil
	}
	result, err = this.Persistence.GetTemplate(id)
	if errors.Is(err, ErrNotFound) {
		return result, false, nil
	}
	return result, true, err
}

func (this *StateRepo) DeleteTemplate(token sc_jwt.Token, id string) (err error) {
	return this.Persistence.DeleteTemplate(id)
}

func (this *StateRepo) ReadTemplates(token sc_jwt.Token) (result []RoutineTemplate, err error) {
	result, err = this.Persistence.GetTemplates()
	if err != nil {
		return
	}
	defaults := []RoutineTemplate{}
	for _, templ := range defaultTemplates {
		defaults = append(defaults, templ)
	}
	result = append(defaults, result...)
	return
}

func (this *StateRepo) UpdateChangeRoutineByTemplate(token sc_jwt.Token, msg UpdateChangeRoutineByTemplateRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	templ, exists, err := this.ReadTemplate(token, msg.TemplId)
	if err != nil || !exists {
		return routine, true, exists, err
	}
	updateRequest := UpdateChangeRoutineRequest{Id: msg.RoutineId, Interval: msg.Interval}
	updateRequest.Code, err = RenderTempl(templ.Template, msg.Parameter)
	if err != nil {
		return routine, true, exists, err
	}
	return this.UpdateChangeRoutine(token, updateRequest)
}

func (this *StateRepo) CreateChangeRoutineByTemplate(token sc_jwt.Token, msg CreateChangeRoutineByTemplateRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	templ, exists, err := this.ReadTemplate(token, msg.TemplId)
	if err != nil || !exists {
		return routine, true, exists, err
	}
	createRequest := CreateChangeRoutineRequest{RefId: msg.RefId, RefType: msg.RefType, Interval: msg.Interval}
	createRequest.Code, err = RenderTempl(templ.Template, msg.Parameter)
	if err != nil {
		return routine, true, exists, err
	}
	return this.CreateChangeRoutine(token, createRequest)
}

func inputOutputSkeletonString(variable model.ContentVariable) (result string, err error) {
	temp := &map[string]interface{}{}
	err = inputOutputSkeletonObj(variable, temp)
	if err != nil {
		return result, err
	}
	b, err := json.Marshal((*temp)[variable.Name])
	return string(b), err
}

func inputOutputSkeletonObj(variable model.ContentVariable, result *map[string]interface{}) (err error) {
	switch variable.Type {
	case model.String:
		(*result)[variable.Name] = ""
	case model.Integer:
		(*result)[variable.Name] = 0
	case model.Float:
		(*result)[variable.Name] = 0.0
	case model.Boolean:
		(*result)[variable.Name] = false
	case model.Structure:
		temp := map[string]interface{}{}
		for _, sub := range variable.SubContentVariables {
			err = inputOutputSkeletonObj(sub, &temp)
			if err != nil {
				return err
			}
		}
		(*result)[variable.Name] = temp

	case model.List:
		temp := map[string]interface{}{}
		for _, sub := range variable.SubContentVariables {
			err = inputOutputSkeletonObj(sub, &temp)
			if err != nil {
				return err
			}
		}
		list := []interface{}{}
		for _, element := range temp {
			list = append(list, element)
		}
		(*result)[variable.Name] = list
	}
	return nil
}
