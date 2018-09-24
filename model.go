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

import "time"

type StateRepo struct {
	Worlds      map[string]World
	Graphs      map[string]Graph
	Persistence PersistenceInterface
	Config      Config
}

type Point struct {
	X float64 `json:"x" bson:"x"`
	Y float64 `json:"y" bson:"y"`
}

type Graph struct {
	Id     string  `json:"id" bson:"id"`
	Name   string  `json:"name" bson:"name"`
	Values []Point `json:"values"`
}

type States map[string]interface{}

type Meta map[string]string

type Code string

type ChangeRoutine struct {
	Interval time.Duration `json:"interval" bson:"interval"`
	Code     Code          `json:"code"`
}

type World struct {
	Id             string          `json:"id" bson:"id"`
	Name           string          `json:"name" bson:"name"`
	Meta           Meta            `json:"meta" bson:"meta"`
	States         States          `json:"states" bson:"states"`
	Rooms          map[string]Room `json:"rooms" bson:"rooms"`
	ChangeRoutines []ChangeRoutine `json:"change_routines" bson:"change_routines"`
}

type Room struct {
	Id             string            `json:"id" bson:"id"`
	Name           string            `json:"name" bson:"name"`
	Meta           Meta              `json:"meta" bson:"meta"`
	States         States            `json:"states" bson:"states"`
	Devices        map[string]Device `json:"devices" bson:"devices"`
	ChangeRoutines []ChangeRoutine   `json:"change_routines" bson:"change_routines"`
}

type Device struct {
	Id             string              `json:"id" bson:"id"`
	Name           string              `json:"name" bson:"name"`
	DeviceType     string              `json:"device_type" bson:"device_type"`
	Meta           Meta                `json:"meta" bson:"meta"`
	SetStates      States              `json:"set_states" bson:"set_states"`
	CurrentStates  States              `json:"current_states" bson:"current_states"`
	ChangeRoutines []ChangeRoutine     `json:"change_routines" bson:"change_routines"`
	Services       map[string]Services `json:"services" bson:"services"`
}

type Services struct {
	Id             string        `json:"id" bson:"id"`
	Name           string        `json:"name" bson:"name"`
	SensorInterval time.Duration `json:"sensor_interval" bson:"sensor_interval"`
	Code           Code          `json:"code"`
}
