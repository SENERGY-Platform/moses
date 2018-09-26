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

import "log"

func (this *StateRepo) getJsWorldApi(world *World) map[string]interface{} {
	return map[string]interface{}{
		"world": this.getJsWorldSubApi(world),
	}
}

func (this *StateRepo) getJsWorldSubApi(world *World) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value interface{}) {
				if world.States == nil {
					world.States = map[string]interface{}{}
				}
				world.States[field] = value
			},
			"get": func(field string) interface{} {
				return world.States[field]
			},
		},
		"getRoom": func(roomid string) map[string]interface{} {
			room, ok := world.Rooms[roomid]
			if !ok {
				log.Println("WARNING: js-api getRoom(), room not found ", roomid)
				return map[string]interface{}{}
			}
			return this.getJsRoomSubApi(room)
		},
	}
}

func (this *StateRepo) getJsRoomApi(world *World, room *Room) map[string]interface{} {
	return map[string]interface{}{
		"world": this.getJsWorldSubApi(world),
		"room":  this.getJsRoomSubApi(room),
	}
}

func (this *StateRepo) getJsRoomSubApi(room *Room) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value interface{}) {
				if room.States == nil {
					room.States = map[string]interface{}{}
				}
				room.States[field] = value
			},
			"get": func(field string) interface{} {
				return room.States[field]
			},
		},
		"getDevice": func(deviceid string) map[string]interface{} {
			device, ok := room.Devices[deviceid]
			if !ok {
				log.Println("WARNING: js-api getDevice(), device not found ", deviceid)
				return map[string]interface{}{}
			}
			return this.getJsDeviceSubApi(device)
		},
	}
}

func (this *StateRepo) getJsDeviceApi(world *World, room *Room, device *Device) map[string]interface{} {
	return map[string]interface{}{
		"world":  this.getJsWorldSubApi(world),
		"room":   this.getJsRoomSubApi(room),
		"device": this.getJsDeviceSubApi(device),
	}
}

func (this *StateRepo) getJsDeviceSubApi(device *Device) map[string]interface{} {
	return map[string]interface{}{
		"state": map[string]interface{}{
			"set": func(field string, value interface{}) {
				if device.States == nil {
					device.States = map[string]interface{}{}
				}
				device.States[field] = value
			},
			"get": func(field string) interface{} {
				return device.States[field]
			},
		},
	}
}

func (this *StateRepo) getJsSensorApi(world *World, room *Room, device *Device, service Service) map[string]interface{} {
	return map[string]interface{}{
		"world":   this.getJsWorldSubApi(world),
		"room":    this.getJsRoomSubApi(room),
		"device":  this.getJsDeviceSubApi(device),
		"service": this.getJsSensorSubApi(device, service),
	}
}

func (this *StateRepo) getJsSensorSubApi(device *Device, service Service) map[string]interface{} {
	return map[string]interface{}{
		"send": func(value interface{}) {
			this.SendSensorData(device, service, value)
		},
		"parameter": func() interface{} {
			return nil
		},
	}
}
