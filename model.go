/*
 * Copyright 2018 InfAI (CC SES)
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

type ChangeRoutineIndexElement struct {
	Id      string
	RefType string // "world" || "room" || "device"
	RefId   string
}

type ChangeRoutine struct {
	Id       string `json:"id" bson:"id"`
	Interval int64  `json:"interval" bson:"interval"`
	Code     string `json:"code" bson:"code"`
}

type RoutineTemplate struct {
	Id          string   `json:"id" bson:"id"`
	Name        string   `json:"name" bson:"name"`
	Description string   `json:"description" bson:"description"`
	Template    string   `json:"template" bson:"template"`
	Parameter   []string `json:"parameter" bson:"parameter"`
}

type World struct {
	Id             string                   `json:"id" bson:"id"`
	Owner          string                   `json:"-" bson:"owner"`
	Name           string                   `json:"name" bson:"name"`
	States         map[string]interface{}   `json:"states" bson:"states"`
	Rooms          map[string]*Room         `json:"rooms" bson:"rooms"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines" bson:"change_routines"`
	mux            sync.Mutex               `json:"-" bson:"-"`
}

type Room struct {
	Id             string                   `json:"id" bson:"id"`
	Name           string                   `json:"name" bson:"name"`
	States         map[string]interface{}   `json:"states" bson:"states"`
	Devices        map[string]*Device       `json:"devices" bson:"devices"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines" bson:"change_routines"`
}

type Device struct {
	Id             string                   `json:"id" bson:"id"`
	Name           string                   `json:"name" bson:"name"`
	ImageUrl       string                   `json:"image_url" bson:"image_url"`
	ExternalTypeId string                   `json:"external_type_id" bson:"external_type_id"`
	ExternalRef    string                   `json:"external_ref" bson:"external_ref"` //platform intern device id; 1:1
	States         map[string]interface{}   `json:"states" bson:"states"`
	ChangeRoutines map[string]ChangeRoutine `json:"change_routines" bson:"change_routines"`
	Services       map[string]Service       `json:"services" bson:"services"`
}

type Service struct {
	Id             string                `json:"id" bson:"id"`
	Name           string                `json:"name" bson:"name"`
	ExternalRef    string                `json:"external_ref" bson:"external_ref"` //platform intern service id, will be used to populate Service.Marshaller and as endpoint for the connector
	SensorInterval int64                 `json:"sensor_interval" bson:"sensor_interval"`
	Code           string                `json:"code"`
	Marshaller     marshaller.Marshaller `json:"-" bson:"marshaller"`
}
