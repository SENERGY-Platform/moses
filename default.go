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

import "github.com/google/uuid"

func getDefaultWorldChangeRoutines() map[string]ChangeRoutine {
	return map[string]ChangeRoutine{}
}

func getDefaultWorldStates(states map[string]interface{}) (result map[string]interface{}) {
	result = map[string]interface{}{
		"temperature": float64(20),
		"humidity":    float64(50),
		"lux":         float64(10000),
		"co-ppm":      float64(1.5),
	}
	for key, value := range states {
		result[key] = value
	}
	return result
}

func getDefaultRoomChangeRoutines() (result map[string]ChangeRoutine, err error) {
	tempId, err := uuid.NewRandom()
	if err != nil {
		return result, err
	}
	humitId, err := uuid.NewRandom()
	if err != nil {
		return result, err
	}
	coId, err := uuid.NewRandom()
	if err != nil {
		return result, err
	}
	return map[string]ChangeRoutine{
		tempId.String(): ChangeRoutine{
			Id:       tempId.String(),
			Interval: 10,
			Code:     default_room_temp_code,
		},
		humitId.String(): ChangeRoutine{
			Id:       humitId.String(),
			Interval: 10,
			Code:     default_room_hum_code,
		},
		coId.String(): ChangeRoutine{
			Id:       coId.String(),
			Interval: 10,
			Code:     default_room_co_code,
		},
	}, nil
}

func getDefaultRoomStates(states map[string]interface{}) (result map[string]interface{}) {
	result = map[string]interface{}{
		"temperature": float64(20),
		"humidity":    float64(50),
		"lux":         float64(50),
		"co-ppm":      float64(1.5),
	}
	for key, value := range states {
		result[key] = value
	}
	return result
}

var defaultTemplates = map[string]RoutineTemplate{
	"default_linear": RoutineTemplate{
		Name: "linear",
		Description: `default linear template
compareState is for example world
compareValue is for example temp
changeState is for example world.getRoom("room_1")
changeValue is for example temp
increment is the value by which the changeValue is incremented ore decremented`,
		Parameter: []string{"compareState", "compareValue", "changeState", "changeValue", "increment"},
		Template: `var targetValue = moses.{{compareState}}.state.get("{{compareValue}}");
var isValue = moses.{{changeState}}.state.get("{{changeValue}}");
if(targetValue > isValue){
	isValue = isValue + {{increment}};
}else if(targetValue < isValue){
	isValue = isValue - {{increment}};
}
isValue = Number(isValue.toFixed(1));
moses.{{changeState}}.state.set("{{changeValue}}", isValue);`,
	},
}

const default_room_temp_code = `var temperature = moses.world.state.get("temperature");
var room_temperature = moses.room.state.get("temperature");
if(temperature > room_temperature){
	room_temperature = room_temperature + 1;
}else if(temperature < room_temperature){
	room_temperature = room_temperature - 1;
}
room_temperature = Number(room_temperature.toFixed(1));
moses.room.state.set("temperature", room_temperature);
`

const default_room_hum_code = `var humidity = moses.world.state.get("humidity");
var room_humidity = moses.room.state.get("humidity");
if(humidity > room_humidity){
	room_humidity = room_humidity + 1;
}else if(humidity < room_humidity){
	room_humidity = room_humidity - 1;
}
room_humidity = Number(room_humidity.toFixed(1));
moses.room.state.set("humidity", room_humidity);
`

const default_room_co_code = `var co = moses.world.state.get("co-ppm");
var room_co = moses.room.state.get("co-ppm");
if(co > room_co){
	room_co = room_co + 0.1;
}else if(co < room_co){
	room_co = room_co - 0.1;
}
room_co = Number(room_co.toFixed(2));
moses.room.state.set("co-ppm", room_co);
`
