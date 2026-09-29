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
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/jsguard"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/robertkrimen/otto"
)

func (this *StateRepo) getJsWorldApi(world *World) map[string]interface{} {
	return map[string]interface{}{
		"world": this.getJsWorldSubApi(world),
	}
}

func (this *StateRepo) getJsWorldSubApi(world *World) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value otto.Value) {
				//value is an otto.Value, not interface{}: otto exports (and recurses
				//over) an interface{} argument before this runs. convertOttoValue
				//reads it in one bounded pass that never exports a composite.
				stored, err := world.convertGuarded(value, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
				if err != nil {
					util.Logger.Warn("the script tried to store a value that cannot be stored, it is dropped", attributes.ErrorKey, err, "field", field)
					return
				}
				if world.States == nil {
					world.States = map[string]interface{}{}
				}
				world.States[field] = stored
				if world != nil {
					err := this.persistWorld(*world)
					if err != nil {
						util.Logger.Error("unable to persist world state", attributes.ErrorKey, err, "world", world.Id, "field", field)
					}
				}
			},
			"get": func(field string) interface{} {
				if world.States == nil {
					world.States = map[string]interface{}{}
				}
				val, ok := world.States[field]
				if !ok {
					world.States[field] = 0
					val = 0
				}
				return readStateValue(field, val)
			},
		},
		"getRoom": func(roomid string) map[string]interface{} {
			room, ok := world.Rooms[roomid]
			if !ok {
				util.Logger.Warn("no room for id found", "id", roomid)
				return map[string]interface{}{}
			}
			return this.getJsRoomSubApi(world, room)
		},
	}
}

func (this *StateRepo) getJsRoomApi(world *World, room *Room) map[string]interface{} {
	return map[string]interface{}{
		"world": this.getJsWorldSubApi(world),
		"room":  this.getJsRoomSubApi(world, room),
	}
}

func (this *StateRepo) getJsRoomSubApi(world *World, room *Room) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value otto.Value) {
				//value is an otto.Value, not interface{}: otto exports (and recurses
				//over) an interface{} argument before this runs. convertOttoValue
				//reads it in one bounded pass that never exports a composite.
				stored, err := world.convertGuarded(value, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
				if err != nil {
					util.Logger.Warn("the script tried to store a value that cannot be stored, it is dropped", attributes.ErrorKey, err, "field", field)
					return
				}
				if room.States == nil {
					room.States = map[string]interface{}{}
				}
				room.States[field] = stored
				if world != nil {
					err := this.persistWorld(*world)
					if err != nil {
						util.Logger.Error("unable to persist world state", attributes.ErrorKey, err, "world", world.Id, "room", room.Id, "field", field)
					}
				}
			},
			"get": func(field string) interface{} {
				if room.States == nil {
					room.States = map[string]interface{}{}
				}
				val, ok := room.States[field]
				if !ok {
					room.States[field] = 0
					val = 0
				}
				return readStateValue(field, val)
			},
		},
		"getDevice": func(deviceid string) map[string]interface{} {
			device, ok := room.Devices[deviceid]
			if !ok {
				util.Logger.Warn("no device for id found", "id", deviceid)
				return map[string]interface{}{}
			}
			return this.getJsDeviceSubApi(world, device)
		},
	}
}

func (this *StateRepo) getJsDeviceApi(world *World, room *Room, device *Device) map[string]interface{} {
	return map[string]interface{}{
		"world":  this.getJsWorldSubApi(world),
		"room":   this.getJsRoomSubApi(world, room),
		"device": this.getJsDeviceSubApi(world, device),
	}
}

func (this *StateRepo) getJsDeviceSubApi(world *World, device *Device) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value otto.Value) {
				//value is an otto.Value, not interface{}: otto exports (and recurses
				//over) an interface{} argument before this runs. convertOttoValue
				//reads it in one bounded pass that never exports a composite.
				stored, err := world.convertGuarded(value, jsguard.MaxStateDepth+1, jsguard.MaxStateNodes)
				if err != nil {
					util.Logger.Warn("the script tried to store a value that cannot be stored, it is dropped", attributes.ErrorKey, err, "field", field)
					return
				}
				if device.States == nil {
					device.States = map[string]interface{}{}
				}
				device.States[field] = stored
				if world != nil {
					err := this.persistWorld(*world)
					if err != nil {
						util.Logger.Error("unable to persist world state", attributes.ErrorKey, err, "world", world.Id, "device", device.Id, "field", field)
					}
				}
			},
			"get": func(field string) interface{} {
				if device.States == nil {
					device.States = map[string]interface{}{}
				}
				val, ok := device.States[field]
				if !ok {
					device.States[field] = 0
					val = 0
				}
				return readStateValue(field, val)
			},
		},
	}
}

func (this *StateRepo) getJsSensorApi(world *World, room *Room, device *Device, service Service) map[string]interface{} {
	return map[string]interface{}{
		"world":   this.getJsWorldSubApi(world),
		"room":    this.getJsRoomSubApi(world, room),
		"device":  this.getJsDeviceSubApi(world, device),
		"service": this.getJsSensorSubApi(world, device, service),
	}
}

func (this *StateRepo) getJsSensorSubApi(world *World, device *Device, service Service) map[string]interface{} {
	return map[string]interface{}{
		"send": func(value otto.Value) {
			//otto.Value, not interface{}: converted in one bounded pass that never
			//exports a composite, so a deep value cannot overflow the exporter
			exported, err := world.convertGuarded(value, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes)
			if err != nil {
				util.Logger.Warn("the script tried to send a value that cannot be sent, it is dropped", attributes.ErrorKey, err)
				return
			}
			this.sendSensorData(device, service, exported)
		},
		"input": nil,
	}
}

func (this *StateRepo) getJsCommandApi(world *World, room *Room, device *Device, cmdMsg interface{}, responder func(respMsg interface{})) map[string]interface{} {
	return map[string]interface{}{
		"world":   this.getJsWorldSubApi(world),
		"room":    this.getJsRoomSubApi(world, room),
		"device":  this.getJsDeviceSubApi(world, device),
		"service": this.getJsCommandSubApi(world, cmdMsg, responder),
	}
}

func (this *StateRepo) getJsCommandSubApi(world *World, cmdMsg interface{}, responder func(respMsg interface{})) interface{} {
	return map[string]interface{}{
		"input": cmdMsg,
		//otto.Value, not interface{}: a deep response would otherwise overflow the
		//exporter before the responder is reached
		"send": func(value otto.Value) {
			exported, err := world.convertGuarded(value, jsguard.MaxNativeWalkDepth, jsguard.MaxNativeWalkNodes)
			if err != nil {
				util.Logger.Warn("the script tried to respond with a value that cannot be sent, it is dropped", attributes.ErrorKey, err)
				return
			}
			responder(exported)
		},
	}
}

// convertGuarded runs one bounded conversion and refuses a nested one, so a
// getter that calls a sink while its holder is converted fails instead of
// stacking a second conversion. The guard is released before the caller persists.
func (this *World) convertGuarded(value otto.Value, maxDepth int, maxNodes int) (interface{}, error) {
	if this == nil {
		return convertOttoValue(value, maxDepth, maxNodes)
	}
	if !this.sink.Enter() {
		return nil, jsguard.ErrSinkReentry
	}
	defer this.sink.Leave()
	return convertOttoValue(value, maxDepth, maxNodes)
}

// plainStateValue returns a fresh plain-data copy of value, or false for a value
// that is not plain data: a Go function bound into one routine's vm would
// otherwise be callable from every other routine, and storing the script's own
// structure would let it change after the check.
func plainStateValue(field string, value interface{}) (interface{}, bool) {
	copied, err := jsguard.CopyPlainData(value)
	if err != nil {
		util.Logger.Warn("the script tried to store a value that is not plain data, it is dropped", attributes.ErrorKey, err, "field", field)
		return nil, false
	}
	return copied, true
}

// readStateValue is the getter's copy; a value past the node budget, which only a
// corrupted state can hold, is logged and read as null.
func readStateValue(field string, value interface{}) interface{} {
	copied, err := jsguard.PlainCopy(value)
	if err != nil {
		util.Logger.Warn("a stored state value is too large to read, it reads as null", attributes.ErrorKey, err, "field", field)
		return nil
	}
	return copied
}
