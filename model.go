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
	"moses/marshaller"
	"sync"
	"time"
)

type Point struct {
	X float64 `json:"x" bson:"x"`
	Y float64 `json:"y" bson:"y"`
}

type Graph struct {
	Id     string  `json:"id" bson:"id"`
	Name   string  `json:"name" bson:"name"`
	Values []Point `json:"values" bson:"values"`
}

type ChangeRoutine struct {
	Id       string        `json:"id" bson:"id"`
	Interval time.Duration `json:"interval" bson:"interval"`
	Code     string        `json:"code" bson:"code"`
}

type RoutineTemplate struct {
	Id          string   `json:"id" bson:"id"`
	Name        string   `json:"name" bson:"name"`
	Description string   `json:"description" bson:"description"`
	Template    string   `json:"template" bson:"template"`
	Parameter   []string `json:"parameter" bson:"parameter"`
}

type World struct {
	Id             string                 `json:"id" bson:"id"`
	Owner          string                 `json:"-" bson:"owner"`
	Name           string                 `json:"name" bson:"name"`
	States         map[string]interface{} `json:"states" bson:"states"`
	Rooms          map[string]*Room       `json:"rooms" bson:"rooms"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines" bson:"change_routines"`
	mux            sync.Mutex             `json:"-" bson:"-"`
}

type Room struct {
	Id             string                 `json:"id" bson:"id"`
	Name           string                 `json:"name" bson:"name"`
	Meta           map[string]string      `json:"meta" bson:"meta"`
	States         map[string]interface{} `json:"states" bson:"states"`
	Devices        map[string]*Device     `json:"devices" bson:"devices"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines" bson:"change_routines"`
}

type Device struct {
	Id             string                 `json:"id" bson:"id"`
	Name           string                 `json:"name" bson:"name"`
	DeviceType     string                 `json:"device_type" bson:"device_type"`
	Meta           map[string]string      `json:"meta" bson:"meta"`
	States         map[string]interface{} `json:"states" bson:"states"`
	ChangeRoutines []ChangeRoutine        `json:"change_routines" bson:"change_routines"`
	Services       map[string]Service     `json:"services" bson:"services"`
}

type Service struct {
	Id             string                `json:"id" bson:"id"`
	Name           string                `json:"name" bson:"name"`
	SensorInterval time.Duration         `json:"sensor_interval" bson:"sensor_interval"`
	Code           string                `json:"code"`
	Marshaller     marshaller.Marshaller `json:"marshaller" bson:"marshaller"`
}
