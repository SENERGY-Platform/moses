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
	"errors"
	"github.com/SENERGY-Platform/moses/lib/jwt"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"github.com/cbroglie/mustache"
	"github.com/globalsign/mgo"
	"github.com/google/uuid"
	"log"
	"strings"
)

func (this *StateRepo) ReadWorlds(jwt jwt.Jwt) (worlds []WorldMsg, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	for _, world := range this.Worlds {
		if world.Owner == jwt.UserId {
			msg, err := world.ToMsg()
			if err != nil {
				return worlds, err
			}
			worlds = append(worlds, msg)
		}
	}
	return
}

func (this *StateRepo) CreateWorld(jwt jwt.Jwt, msg CreateWorldRequest) (world WorldMsg, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return world, err
	}
	world = WorldMsg{Id: uid.String(), Name: msg.Name, States: getDefaultWorldStates(msg.States), Owner: jwt.UserId, ChangeRoutines: getDefaultWorldChangeRoutines()}
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) ReadWorld(jwt jwt.Jwt, id string) (world WorldMsg, access bool, exists bool, err error) {
	world, exists, err = this.DevGetWorld(id)
	if err != nil || !exists {
		return
	}
	if world.Owner != jwt.UserId {
		return WorldMsg{}, false, exists, err
	}
	return world, true, true, err
}

func (this *StateRepo) UpdateWorld(jwt jwt.Jwt, msg UpdateWorldRequest) (world WorldMsg, access bool, exists bool, err error) {
	world, access, exists, err = this.ReadWorld(jwt, msg.Id)
	if err != nil || !access || !exists {
		world = WorldMsg{}
		return
	}
	world.Name = msg.Name
	world.States = msg.States
	world.ChangeRoutines = msg.ChangeRoutines
	err = this.DevUpdateWorld(world)
	return
}

func (this *StateRepo) DeleteWorld(jwt jwt.Jwt, id string) (access bool, exists bool, err error) {
	world, exists, err := this.DevGetWorld(id)
	if err != nil || !exists {
		log.Println("ERROR:", err, exists)
		return access, exists, err
	}
	if world.Owner != jwt.UserId {
		log.Println("ERROR: access denied", world.Owner, jwt.UserId)
		return false, exists, err
	}
	err = this.DevDeleteWorld(id)
	return true, exists, err
}

func (this *StateRepo) ReadRoom(jwt jwt.Jwt, id string) (room RoomResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	world, exists := this.roomWorldIndex[id]
	if !exists {
		log.Println("WARNING: room world index id not found", id, this.roomWorldIndex)
		return room, false, exists, nil
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	if world.Owner != jwt.UserId {
		log.Println("WARNING: room access denied", world.Owner, " != ", jwt.UserId)
		return room, false, exists, nil
	}
	room.World = world.Id
	room.Room, err = world.Rooms[id].ToMsg()
	return room, true, true, err
}

func (this *StateRepo) UpdateRoom(jwt jwt.Jwt, msg UpdateRoomRequest) (room RoomResponse, access bool, exists bool, err error) {
	room, access, exists, err = this.ReadRoom(jwt, msg.Id)
	if err != nil || !access || !exists {
		log.Println("WARNING: update world", access, exists, err)
		return
	}
	room.Room.States = msg.States
	room.Room.Name = msg.Name
	room.Room.Id = msg.Id
	room.Room.ChangeRoutines = msg.ChangeRoutines
	err = this.DevUpdateRoom(room.World, room.Room)
	return
}

func (this *StateRepo) CreateRoom(jwt jwt.Jwt, msg CreateRoomRequest) (room RoomResponse, access bool, worldExists bool, err error) {
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
	room.Room.States = getDefaultRoomStates(msg.States)
	room.World = worldMsg.Id
	room.Room.ChangeRoutines, err = getDefaultRoomChangeRoutines()
	if err != nil {
		return room, true, true, err
	}
	err = this.DevUpdateRoom(room.World, room.Room)
	return room, true, true, err
}

func (this *StateRepo) DeleteRoom(jwt jwt.Jwt, id string) (room RoomResponse, access bool, exists bool, err error) {
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
	return room, true, exists, err
}

func (this *StateRepo) ReadDevice(jwt jwt.Jwt, id string) (device DeviceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	world, exists := this.deviceWorldIndex[id]
	if !exists {
		return device, false, exists, nil
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	if world.Owner != jwt.UserId {
		return device, false, exists, nil
	}

	room, exists := this.deviceRoomIndex[id]
	if !exists {
		return device, false, exists, errors.New("inconsistent deviceRoomIndex")
	}

	device.World = world.Id
	device.Room = room.Id
	device.Device, err = room.Devices[id].ToMsg()
	return device, true, true, err
}

func (this *StateRepo) CreateDevice(jwt jwt.Jwt, msg CreateDeviceRequest) (device DeviceResponse, access bool, worldAndExists bool, err error) {
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
	device.Device.ChangeRoutines = map[string]ChangeRoutine{}
	err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	return device, true, true, err
}

func (this *StateRepo) UpdateDevice(jwt jwt.Jwt, msg UpdateDeviceRequest) (device DeviceResponse, access bool, exists bool, err error) {
	device, access, exists, err = this.ReadDevice(jwt, msg.Id)
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
		device.Device.Services[key], err = this.PopulateServiceService(jwt, UpdateServiceRequest{Id: value.Id, Code: value.Code, SensorInterval: value.SensorInterval, ExternalRef: value.ExternalRef, Name: value.Name})
		if err != nil {
			log.Println("ERROR:", err)
			return device, true, true, err
		}
	}
	err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	return device, true, true, err
}

func (this *StateRepo) DeleteDevice(jwt jwt.Jwt, id string) (device DeviceResponse, access bool, exists bool, err error) {
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
	if err == nil {
		this.DeleteExternalDevice(jwt, device.Device.ExternalRef)
	}
	return device, true, true, err
}

func (this *StateRepo) ReadService(jwt jwt.Jwt, id string) (service ServiceResponse, access bool, exists bool, err error) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	devicep, exists := this.serviceDeviceIndex[id]
	if !exists {
		return service, false, exists, nil
	}
	world, exists := this.deviceWorldIndex[devicep.Id]
	if !exists {
		return service, false, exists, errors.New("inconsistent deviceWorldIndex")
	}
	world.mux.Lock()
	defer world.mux.Unlock()
	if world.Owner != jwt.UserId {
		return service, false, exists, nil
	}

	room, exists := this.deviceRoomIndex[devicep.Id]
	if !exists {
		return service, false, exists, errors.New("inconsistent deviceRoomIndex")
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
	return service, true, true, err
}

func (this *StateRepo) CreateService(jwt jwt.Jwt, msg CreateServiceRequest) (service ServiceResponse, access bool, worldAndExists bool, err error) {
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
	if device.Device.Services == nil {
		device.Device.Services = map[string]Service{}
	}
	device.Device.Services[service.Service.Id], err = this.PopulateServiceService(jwt, service.Service)
	if err != nil {
		return service, access, worldAndExists, err
	}
	err = this.DevUpdateDevice(service.World, service.Room, device.Device)
	return service, true, true, err
}

func (this *StateRepo) PopulateServiceService(jwt jwt.Jwt, serviceMsg UpdateServiceRequest) (service Service, err error) {
	service.Id = serviceMsg.Id
	service.Name = serviceMsg.Name
	service.SensorInterval = serviceMsg.SensorInterval
	service.Code = serviceMsg.Code
	service.ExternalRef = serviceMsg.ExternalRef
	return
}

func (this *StateRepo) UpdateService(jwt jwt.Jwt, msg UpdateServiceRequest) (service ServiceResponse, access bool, exists bool, err error) {
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
	return service, true, true, err
}

func (this *StateRepo) DeleteService(jwt jwt.Jwt, id string) (service ServiceResponse, access bool, exists bool, err error) {
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
	return service, true, true, err
}

func (this *StateRepo) CreateDeviceByType(jwt jwt.Jwt, msg CreateDeviceByTypeRequest) (result DeviceResponse, access bool, worldAndExists bool, err error) {
	room := RoomResponse{}
	room, access, worldAndExists, err = this.ReadRoom(jwt, msg.Room)
	if err != nil || !access || !worldAndExists {
		return result, access, worldAndExists, err
	}
	services, err := this.prepareServices(jwt, msg.DeviceTypeId)
	if err != nil {
		return result, access, worldAndExists, err
	}
	externalDevice, err := this.GenerateExternalDevice(jwt, msg)
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
	err = this.DevUpdateDevice(result.World, result.Room, result.Device)
	return result, true, true, err
}

func (this *StateRepo) prepareServices(jwt jwt.Jwt, deviceTypeId string) (result map[string]Service, err error) {
	result = map[string]Service{}
	devicetype, err := this.GetIotDeviceType(jwt, deviceTypeId)
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

func (this *StateRepo) CreateChangeRoutine(jwt jwt.Jwt, msg CreateChangeRoutineRequest) (result ChangeRoutineResponse, access bool, exists bool, err error) {
	uid, err := uuid.NewRandom()
	if err != nil {
		return result, access, exists, err
	}
	routine := ChangeRoutine{Interval: msg.Interval, Code: msg.Code, Id: uid.String()}
	result = ChangeRoutineResponse{Id: routine.Id, Code: routine.Code, Interval: routine.Interval, RefId: msg.RefId, RefType: msg.RefType}
	switch msg.RefType {
	case "world":
		world, access, exists, err := this.ReadWorld(jwt, msg.RefId)
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if world.ChangeRoutines == nil {
			world.ChangeRoutines = map[string]ChangeRoutine{}
		}
		world.ChangeRoutines[routine.Id] = routine
		err = this.DevUpdateWorld(world)
	case "room":
		room, access, exists, err := this.ReadRoom(jwt, msg.RefId)
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if room.Room.ChangeRoutines == nil {
			room.Room.ChangeRoutines = map[string]ChangeRoutine{}
		}
		room.Room.ChangeRoutines[routine.Id] = routine
		err = this.DevUpdateRoom(room.World, room.Room)
	case "device":
		device, access, exists, err := this.ReadDevice(jwt, msg.RefId)
		if err != nil || !access || !exists {
			return result, access, exists, err
		}
		if device.Device.ChangeRoutines == nil {
			device.Device.ChangeRoutines = map[string]ChangeRoutine{}
		}
		device.Device.ChangeRoutines[routine.Id] = routine
		err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	default:
		err = errors.New("unknown ref type")
	}
	return result, true, true, err
}

func (this *StateRepo) UpdateChangeRoutine(jwt jwt.Jwt, msg UpdateChangeRoutineRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	routine, access, exists, err = this.ReadChangeRoutine(jwt, msg.Id)
	if err != nil || !access || !exists {
		return routine, access, exists, err
	}
	changeRoutine := ChangeRoutine{Interval: msg.Interval, Code: msg.Code, Id: msg.Id}
	routine.Code = changeRoutine.Code
	routine.Interval = changeRoutine.Interval
	switch routine.RefType {
	case "world":
		world, access, exists, err := this.ReadWorld(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		world.ChangeRoutines[msg.Id] = changeRoutine
		err = this.DevUpdateWorld(world)
	case "room":
		room, access, exists, err := this.ReadRoom(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		room.Room.ChangeRoutines[msg.Id] = changeRoutine
		err = this.DevUpdateRoom(room.World, room.Room)
	case "device":
		device, access, exists, err := this.ReadDevice(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		device.Device.ChangeRoutines[msg.Id] = changeRoutine
		err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	default:
		err = errors.New("unknown ref type")
	}
	return routine, true, true, err
}

func (this *StateRepo) getChangeRoutineFromIndex(id string) (routine ChangeRoutineIndexElement, exists bool) {
	this.mux.RLock()
	defer this.mux.RUnlock()
	routine, exists = this.changeRoutineIndex[id]
	return
}

func (this *StateRepo) ReadChangeRoutine(jwt jwt.Jwt, id string) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	index, exists := this.getChangeRoutineFromIndex(id)
	if !exists {
		return routine, access, exists, err
	}
	routine.RefType = index.RefType
	routine.RefId = index.RefId
	routine.Id = id
	switch routine.RefType {
	case "world":
		world, access, exists, err := this.ReadWorld(jwt, routine.RefId)
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
		room, access, exists, err := this.ReadRoom(jwt, routine.RefId)
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
		device, access, exists, err := this.ReadDevice(jwt, routine.RefId)
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

func (this *StateRepo) DeleteChangeRoutine(jwt jwt.Jwt, id string) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	routine, access, exists, err = this.ReadChangeRoutine(jwt, id)
	if err != nil || !access || !exists {
		return
	}
	switch routine.RefType {
	case "world":
		world, access, exists, err := this.ReadWorld(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		err = this.DevUpdateWorld(world)
	case "room":
		room, access, exists, err := this.ReadRoom(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		err = this.DevUpdateRoom(room.World, room.Room)
	case "device":
		device, access, exists, err := this.ReadDevice(jwt, routine.RefId)
		if err != nil || !access || !exists {
			return routine, access, exists, err
		}
		err = this.DevUpdateDevice(device.World, device.Room, device.Device)
	default:
		err = errors.New("unknown ref type")
	}
	return routine, true, true, err
}

func (this *StateRepo) CreateTemplate(jwt jwt.Jwt, request CreateTemplateRequest) (result RoutineTemplate, err error) {
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

func (this *StateRepo) UpdateTemplate(jwt jwt.Jwt, request UpdateTemplateRequest) (result RoutineTemplate, exists bool, err error) {
	result, err = this.Persistence.GetTemplate(request.Id)
	if err == mgo.ErrNotFound {
		log.Println("WARNING: template not found", request.Id)
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

func (this *StateRepo) ReadTemplate(jwt jwt.Jwt, id string) (result RoutineTemplate, exists bool, err error) {
	result, ok := defaultTemplates[id]
	if ok {
		return result, true, nil
	}
	result, err = this.Persistence.GetTemplate(id)
	if err == mgo.ErrNotFound {
		return result, false, nil
	}
	return result, true, err
}

func (this *StateRepo) DeleteTemplate(jwt jwt.Jwt, id string) (err error) {
	return this.Persistence.DeleteTemplate(id)
}

func (this *StateRepo) ReadTemplates(jwt jwt.Jwt) (result []RoutineTemplate, err error) {
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

func (this *StateRepo) UpdateChangeRoutineByTemplate(jwt jwt.Jwt, msg UpdateChangeRoutineByTemplateRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	templ, exists, err := this.ReadTemplate(jwt, msg.TemplId)
	if err != nil || !exists {
		return routine, true, exists, err
	}
	updateRequest := UpdateChangeRoutineRequest{Id: msg.RoutineId, Interval: msg.Interval}
	updateRequest.Code, err = RenderTempl(templ.Template, msg.Parameter)
	return this.UpdateChangeRoutine(jwt, updateRequest)
}

func (this *StateRepo) CreateChangeRoutineByTemplate(jwt jwt.Jwt, msg CreateChangeRoutineByTemplateRequest) (routine ChangeRoutineResponse, access bool, exists bool, err error) {
	templ, exists, err := this.ReadTemplate(jwt, msg.TemplId)
	if err != nil || !exists {
		return routine, true, exists, err
	}
	createRequest := CreateChangeRoutineRequest{RefId: msg.RefId, RefType: msg.RefType, Interval: msg.Interval}
	createRequest.Code, err = RenderTempl(templ.Template, msg.Parameter)
	return this.CreateChangeRoutine(jwt, createRequest)
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
