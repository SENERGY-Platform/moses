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
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/julienschmidt/httprouter"
)

func StartApi(config Config, staterepo *StateRepo) {
	httpHandler := getRoutes(config, staterepo)
	logger := Logger(httpHandler, config.LogLevel)
	log.Println(http.ListenAndServe(":"+config.ServerPort, logger))
}

func getRoutes(config Config, state *StateRepo) *httprouter.Router {
	router := httprouter.New()

	// PUTS only work on current level. sublevel will be preserved ( for example, put on room wont change devices of the room or change what devices the room has )
	// empty on list == []; not nil
	// states are managed by crud of parent entity

	// C	= 	POST
	// R	= 	GET
	// U 	= 	PUT
	// D	= 	DELETE

	// GET /worlds
	router.GET("/worlds", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.ReadWorlds(jwt)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /world
	router.PUT("/world", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateWorld(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /world
	router.POST("/world", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.CreateWorld(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /world/:wid
	router.GET("/world/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadWorld(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /world/:wid
	router.DELETE("/world/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		access, exists, err := state.DeleteWorld(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		fmt.Fprint(resp, "ok")
	})

	// PUT /room
	router.PUT("/room", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateRoom(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /room
	router.POST("/room", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldExists, err := state.CreateRoom(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !worldExists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /room/:wid
	router.GET("/room/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadRoom(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /room/:wid
	router.DELETE("/room/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteRoom(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		fmt.Fprint(resp, "ok")
	})

	// PUT /device
	router.PUT("/device", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateDeviceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateDevice(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /device
	router.POST("/device", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateDeviceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateDevice(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !worldAndRoomExists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world or room id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /device/:id
	router.GET("/device/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadDevice(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /device/:wid
	router.DELETE("/device/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteDevice(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		fmt.Fprint(resp, "ok")
	})

	// PUT /service
	router.PUT("/service", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateService(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /service
	router.POST("/service", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateService(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !worldAndRoomExists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world or room id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /service/:wid
	router.GET("/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadService(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /service/:wid
	router.DELETE("/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteService(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		fmt.Fprint(resp, "ok")
	})

	// POST /device/bydevicetype
	router.POST("/device/bydevicetype", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateDeviceByTypeRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateDeviceByType(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !worldAndRoomExists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world or room id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /devicetypes
	// returns list of device type ids which use the moses protocol
	// to get DeviceType objects you can call the permsearch endpoint POST /ids/select/:resource_kind/:right ; /ids/select/:resource_kind/:right/:limit/:offset/:orderfeature/:direction or by requesting the iot repository
	router.GET("/devicetypes", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.GetDeviceTypesIds(jwt)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /changeroutine					//{id:"", interval: 0, code:""}
	router.PUT("/changeroutine", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateChangeRoutine(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /changeroutine					//{ref_type:"workd|room|device", ref_id: "", interval: 0, code:""}
	router.POST("/changeroutine", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.CreateChangeRoutine(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world, room or device id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /changeroutine/:routineid
	router.GET("/changeroutine/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadChangeRoutine(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /changeroutine/:routineid
	router.DELETE("/changeroutine/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteChangeRoutine(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		fmt.Fprint(resp, "ok")
	})

	// PUT /routinetemplate					// body: {id: "", name: "", desc: "", templ:""}
	router.PUT("/routinetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		if !isAdmin(jwt) {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		msg := UpdateTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, exists, err := state.UpdateTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}

		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /routinetemplate				// body: {name: "", desc: "", templ:""}
	router.POST("/routinetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: jwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		if !isAdmin(jwt) {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		msg := CreateTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: jsondecode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.CreateTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: create", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: jsonencode", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /routinetemplate/:id			// body: {id: "", name: "", desc: "", templ:"", parameter: [""]}
	router.GET("/routinetemplate/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, exists, err := state.ReadTemplate(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /routinetemplates			// contains default templates created by moses
	router.GET("/routinetemplates", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.ReadTemplates(jwt)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /routinetemplate/:id
	router.DELETE("/routinetemplate/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		if !isAdmin(jwt) {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		id := params.ByName("id")
		err = state.DeleteTemplate(jwt, id)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}

		fmt.Fprint(resp, "ok")
	})

	// POST /usetemplate 			// body: {ref_type:"workd|room|device", ref_id: "", templ_id: "", name: "", desc: "", parameter: {<<param_name>>: <<param_value>>}}
	router.POST("/usetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.CreateChangeRoutineByTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown world, room or device id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /usetemplate 			// body: {id: "", templ_id: "", name: "", desc: "", interval:0, parameter: {<<param_name>>: <<param_value>>}}
	router.PUT("/usetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateChangeRoutineByTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		if !access {
			log.Println("WARNING: user access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		if !exists {
			log.Println("WARNING: 404")
			http.Error(resp, "unknown id", http.StatusNotFound)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	/////////////////////////////////////////////////////////////////////////////////////////////////////
	///////////////////////////////////////	 ADMIN - API  ///////////////////////////////////////////////
	/////////////////////////////////////////////////////////////////////////////////////////////////////

	router.GET("/admin/initiot", func(resp http.ResponseWriter, req *http.Request, ps httprouter.Params) {
		jwt, err := GetJwt(req)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		if !isAdmin(jwt) {
			log.Println("WARNING: access denied")
			http.Error(resp, "access denied", http.StatusUnauthorized)
			return
		}
		protocol, err := state.EnsureMosesProtocol(jwt.Impersonate)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
			return
		}

		b, err := json.Marshal(protocol)
		if err != nil {
			log.Println("ERROR: ", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	/////////////////////////////////////////////////////////////////////////////////////////////////////
	///////////////////////////////////////	 DEV - API  /////////////////////////////////////////////////
	/////////////////////////////////////////////////////////////////////////////////////////////////////

	if config.DevApi == "true" {
		router.GET("/dev/worlds", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			b, err := json.Marshal(state.Worlds)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.GET("/dev/world/:worldid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			world, ok := state.Worlds[ps.ByName("worldid")]
			if !ok {
				log.Println("404")
				http.Error(w, "not found", 404)
				return
			}
			b, err := json.Marshal(world)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.GET("/dev/world/:worldid/rooms", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			world, ok := state.Worlds[ps.ByName("worldid")]
			if !ok {
				log.Println("404")
				http.Error(w, "world not found", 404)
				return
			}
			b, err := json.Marshal(world.Rooms)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.GET("/dev/world/:worldid/room/:roomid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			world, ok := state.Worlds[ps.ByName("worldid")]
			if !ok {
				log.Println("404")
				http.Error(w, "world not found", 404)
				return
			}
			room, ok := world.Rooms[ps.ByName("roomid")]
			if !ok {
				log.Println("404")
				http.Error(w, "room not found", 404)
				return
			}
			b, err := json.Marshal(room)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.GET("/dev/world/:worldid/room/:roomid/devices", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			world, ok := state.Worlds[ps.ByName("worldid")]
			if !ok {
				log.Println("404")
				http.Error(w, "world not found", 404)
				return
			}
			room, ok := world.Rooms[ps.ByName("roomid")]
			if !ok {
				log.Println("404")
				http.Error(w, "room not found", 404)
				return
			}
			b, err := json.Marshal(room.Devices)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.GET("/dev/world/:worldid/room/:roomid/device/:deviceid", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			state.mux.RLock()
			defer state.mux.RUnlock()
			world, ok := state.Worlds[ps.ByName("worldid")]
			if !ok {
				log.Println("404")
				http.Error(w, "world not found", 404)
				return
			}
			room, ok := world.Rooms[ps.ByName("roomid")]
			if !ok {
				log.Println("404")
				http.Error(w, "room not found", 404)
				return
			}
			device, ok := room.Devices[ps.ByName("deviceid")]
			if !ok {
				log.Println("404")
				http.Error(w, "device not found", 404)
				return
			}
			b, err := json.Marshal(device)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
			} else {
				fmt.Fprint(w, string(b))
			}
		})

		router.PUT("/dev/world", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			world := WorldMsg{}
			err := json.NewDecoder(r.Body).Decode(&world)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 400)
				return
			}
			err = state.DevUpdateWorld(world)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
				return
			}
			fmt.Fprint(w, "ok")
		})

		router.PUT("/dev/world/:worldid/room", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			room := RoomMsg{}
			err := json.NewDecoder(r.Body).Decode(&room)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 400)
				return
			}
			err = state.DevUpdateRoom(ps.ByName("worldid"), room)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
				return
			}
			fmt.Fprint(w, "ok")
		})

		router.PUT("/dev/world/:worldid/room/:roomid/device", func(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
			device := DeviceMsg{}
			err := json.NewDecoder(r.Body).Decode(&device)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 400)
				return
			}
			err = state.DevUpdateDevice(ps.ByName("worldid"), ps.ByName("roomid"), device)
			if err != nil {
				log.Println("ERROR: ", err)
				http.Error(w, err.Error(), 500)
				return
			}
			fmt.Fprint(w, "ok")
		})
	}

	/////////////////////////////////////////////////////////////////////////

	return router
}
