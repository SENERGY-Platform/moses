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
	"fmt"
	"log"
	"time"
)

func (this *StateRepo) StartWorld(world *World) (tickers []*time.Ticker, stops []chan bool, err error) {
	for _, routine := range world.ChangeRoutines {
		this.changeRoutineIndex[routine.Id] = ChangeRoutineIndexElement{Id: routine.Id, RefType: "world", RefId: world.Id}
		if routine.Interval > 0 {
			ticker, stop := startChangeRoutine(
				routine,
				this.getJsWorldApi(world),
				this.Config.JsTimeout,
				world.mux,
				fmt.Sprintf("world:%s, owner:%s", world.Name, world.Owner))
			tickers = append(tickers, ticker)
			stops = append(stops, stop)
		}
	}
	for _, room := range world.Rooms {
		roomtickers, roomstops, err := this.StartRoom(world, room)
		if err != nil {
			return tickers, stops, err
		}
		tickers = append(tickers, roomtickers...)
		stops = append(stops, roomstops...)
	}
	return
}

func (this *StateRepo) StartRoom(world *World, room *Room) (tickers []*time.Ticker, stops []chan bool, err error) {
	this.roomWorldIndex[room.Id] = world
	for _, routine := range room.ChangeRoutines {
		this.changeRoutineIndex[routine.Id] = ChangeRoutineIndexElement{Id: routine.Id, RefType: "room", RefId: room.Id}
		if routine.Interval > 0 {
			ticker, stop := startChangeRoutine(
				routine,
				this.getJsRoomApi(world, room),
				this.Config.JsTimeout,
				world.mux,
				fmt.Sprintf("world: %s, room:%s, owner:%s", world.Name, room.Name, world.Owner))
			tickers = append(tickers, ticker)
			stops = append(stops, stop)
		}
	}
	for _, device := range room.Devices {
		roomtickers, roomstops, err := this.StartDevice(world, room, device)
		if err != nil {
			return tickers, stops, err
		}
		tickers = append(tickers, roomtickers...)
		stops = append(stops, roomstops...)
	}
	return
}

func (this *StateRepo) StartDevice(world *World, room *Room, device *Device) (tickers []*time.Ticker, stops []chan bool, err error) {
	err = this.StateLogger.LogDeviceConnect(device.ExternalRef)
	if err != nil {
		log.Println("WARNING: unable to log device as online", err)
	}
	this.externalRefDeviceIndex[device.ExternalRef] = device
	this.deviceRoomIndex[device.Id] = room
	this.deviceWorldIndex[device.Id] = world
	for _, routine := range device.ChangeRoutines {
		this.changeRoutineIndex[routine.Id] = ChangeRoutineIndexElement{Id: routine.Id, RefType: "device", RefId: device.Id}
		if routine.Interval > 0 {
			ticker, stop := startChangeRoutine(
				routine,
				this.getJsDeviceApi(world, room, device),
				this.Config.JsTimeout,
				world.mux,
				fmt.Sprintf("world: %s, room:%s, device:%s, owner:%s", world.Name, room.Name, device.Name, world.Owner))
			tickers = append(tickers, ticker)
			stops = append(stops, stop)
		}
	}
	for _, service := range device.Services {
		roomtickers, roomstops, err := this.StartService(world, room, device, service)
		if err != nil {
			return tickers, stops, err
		}
		tickers = append(tickers, roomtickers...)
		stops = append(stops, roomstops...)
	}
	return
}

func (this *StateRepo) StartService(world *World, room *Room, device *Device, service Service) (tickers []*time.Ticker, stops []chan bool, err error) {
	this.serviceDeviceIndex[service.Id] = device
	if service.SensorInterval > 0 {
		ticker, stop := startChangeRoutine(
			ChangeRoutine{Interval: service.SensorInterval, Code: service.Code},
			this.getJsSensorApi(world, room, device, service),
			this.Config.JsTimeout,
			world.mux,
			fmt.Sprintf("world: %s, room:%s, device:%s, service:%s, owner:%s", world.Name, room.Name, device.Name, service.Name, world.Owner))
		tickers = append(tickers, ticker)
		stops = append(stops, stop)
	}
	return
}
