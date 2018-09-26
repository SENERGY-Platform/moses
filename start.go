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
	"time"
)

func (this *StateRepo) StartWorld(world *World) (tickers []*time.Ticker, stops []chan bool, err error) {
	for _, routine := range world.ChangeRoutines {
		ticker, stop := startChangeRoutine(routine, this.getJsWorldApi(world), this.Config.JsTimeout, &world.mux)
		tickers = append(tickers, ticker)
		stops = append(stops, stop)
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
	for _, routine := range room.ChangeRoutines {
		ticker, stop := startChangeRoutine(routine, this.getJsRoomApi(world, room), this.Config.JsTimeout, &world.mux)
		tickers = append(tickers, ticker)
		stops = append(stops, stop)
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
	this.deviceIndex[device.Id] = device
	this.deviceRoomIndex[device.Id] = room
	this.deviceWorldIndex[device.Id] = world
	for _, routine := range device.ChangeRoutines {
		ticker, stop := startChangeRoutine(routine, this.getJsDeviceApi(world, room, device), this.Config.JsTimeout, &world.mux)
		tickers = append(tickers, ticker)
		stops = append(stops, stop)
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
	if service.SensorInterval > 0 {
		ticker, stop := startChangeRoutine(ChangeRoutine{Interval: service.SensorInterval, Code: service.Code}, this.getJsSensorApi(world, room, device, service), this.Config.JsTimeout, &world.mux)
		tickers = append(tickers, ticker)
		stops = append(stops, stop)
	}
	return
}
