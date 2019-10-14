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

package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/julienschmidt/httprouter"
)

func StartApi(ctx context.Context, config Config, staterepo *StateRepo) {
	httpHandler := getRoutes(config, staterepo)
	logger := Logger(httpHandler, config.LogLevel)
	server := &http.Server{Addr: ":" + config.ServerPort, Handler: logger, WriteTimeout: 10 * time.Second, ReadTimeout: 2 * time.Second, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		log.Println("Listening on ", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("ERROR: api server error", err)
			log.Fatal(err)
		}
	}()
	go func() {
		<-ctx.Done()
		log.Println("DEBUG: api shutdown", server.Shutdown(context.Background()))
	}()
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
			log.Println("ERROR: GET/worlds GetJwt()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.ReadWorlds(jwt)
		if err != nil {
			log.Println("ERROR: GET/worlds ReadWorlds()", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: GET/worlds  Marshal();", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /world
	router.PUT("/world", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT/world GetJwt()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT/world Decode()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateWorld(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT/world UpdateWorld()", err)
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
			log.Println("ERROR: PUT/world Marshal()", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /world
	router.POST("/world", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /world GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /world Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.CreateWorld(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /world CreateWorld", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: POST /world Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /world/:wid
	router.GET("/world/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /world/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadWorld(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /world/:id ReadWorld", err)
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
			log.Println("ERROR: GET /world/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /world/:wid
	router.DELETE("/world/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /world/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		access, exists, err := state.DeleteWorld(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /world/:id DeleteWorld", err)
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
			log.Println("ERROR: PUT /room GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /room Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateRoom(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /room UpdateRoom", err)
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
			log.Println("ERROR: PUT /room Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /room
	router.POST("/room", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /room GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /room Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldExists, err := state.CreateRoom(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /room CreateRoom", err)
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
			log.Println("ERROR: POST /room Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /room/:wid
	router.GET("/room/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /room/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadRoom(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /room/:id ReadRoom", err)
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
			log.Println("ERROR: GET /room/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /room/:wid
	router.DELETE("/room/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /room/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteRoom(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /room/:id DeleteRoom", err)
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
			log.Println("ERROR: PUT /device GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateDeviceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /device Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateDevice(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /device UpdateDevice", err)
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
			log.Println("ERROR: PUT /device Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /device
	router.POST("/device", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /device GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateDeviceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /device Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateDevice(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /device CreateDevice", err)
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
			log.Println("ERROR: POST /device Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /device/:id
	router.GET("/device/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /device/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadDevice(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /device/:id ReadDevice", err)
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
			log.Println("ERROR: GET /device/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /device/:wid
	router.DELETE("/device/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /device/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteDevice(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /device/:id DeleteDevice", err)
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
			log.Println("ERROR: PUT /service GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /service Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateService(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /service UpdateService", err)
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
			log.Println("ERROR: PUT /service Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /service
	router.POST("/service", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /service GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /service Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateService(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /service CreateService", err)
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
			log.Println("ERROR: POST /service Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /service/:wid
	router.GET("/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadService(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /service/:id ReadService", err)
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
			log.Println("ERROR: GET /service/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /service/:wid
	router.DELETE("/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteService(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /service/:id DeleteService", err)
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

	router.POST("/run/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		id := params.ByName("id")
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		_, access, exists, err := state.ReadService(jwt, id)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id ReadService", err)
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
		var msg interface{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.RunService(id, msg)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id RunService", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /device/bydevicetype
	router.POST("/device/bydevicetype", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /device/bydevicetype GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateDeviceByTypeRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: /device/bydevicetype Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := state.CreateDeviceByType(jwt, msg)
		if err != nil {
			log.Println("ERROR: /device/bydevicetype CreateDeviceByType", err)
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
			log.Println("ERROR: /device/bydevicetype Marshal", err)
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
			log.Println("ERROR: GET /devicetypes GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.GetMosesDeviceTypesIds(jwt)
		if err != nil {
			log.Println("ERROR: GET /devicetypes GetJwt", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: GET /devicetypes Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /changeroutine					//{id:"", interval: 0, code:""}
	router.PUT("/changeroutine", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /changeroutine GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /changeroutine Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateChangeRoutine(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /changeroutine UpdateChangeRoutine", err)
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
			log.Println("ERROR: PUT /changeroutine Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// POST /changeroutine					//{ref_type:"workd|room|device", ref_id: "", interval: 0, code:""}
	router.POST("/changeroutine", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /changeroutine GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /changeroutine Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.CreateChangeRoutine(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /changeroutine CreateChangeRoutine", err)
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
			log.Println("ERROR: POST /changeroutine Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /changeroutine/:routineid
	router.GET("/changeroutine/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /changeroutine/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := state.ReadChangeRoutine(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /changeroutine/:id ReadChangeRoutine", err)
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
			log.Println("ERROR: GET /changeroutine/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /changeroutine/:routineid
	router.DELETE("/changeroutine/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /changeroutine/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := state.DeleteChangeRoutine(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /changeroutine/:id DeleteChangeRoutine", err)
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
			log.Println("ERROR: PUT /routinetemplate GetJwt", err)
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
			log.Println("ERROR: PUT /routinetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, exists, err := state.UpdateTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /routinetemplate UpdateTemplate", err)
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
			log.Println("ERROR: PUT /routinetemplate Marshal", err)
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
			log.Println("ERROR: GET /routinetemplate/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, exists, err := state.ReadTemplate(jwt, id)
		if err != nil {
			log.Println("ERROR: GET /routinetemplate/:id ReadTemplate", err)
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
			log.Println("ERROR: GET /routinetemplate/:id Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// GET /routinetemplates			// contains default templates created by moses
	router.GET("/routinetemplates", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /routinetemplates GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := state.ReadTemplates(jwt)
		if err != nil {
			log.Println("ERROR: GET /routinetemplates ReadTemplates", err)
			http.Error(resp, err.Error(), 500)
			return
		}
		b, err := json.Marshal(result)
		if err != nil {
			log.Println("ERROR: GET /routinetemplates Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// DELETE /routinetemplate/:id
	router.DELETE("/routinetemplate/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /routinetemplate/:id GetJwt", err)
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
			log.Println("ERROR: DELETE /routinetemplate/:id DeleteTemplate", err)
			http.Error(resp, err.Error(), 500)
			return
		}

		fmt.Fprint(resp, "ok")
	})

	// POST /usetemplate 			// body: {ref_type:"workd|room|device", ref_id: "", templ_id: "", name: "", desc: "", parameter: {<<param_name>>: <<param_value>>}}
	router.POST("/usetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /usetemplate GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := CreateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /usetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.CreateChangeRoutineByTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: POST /usetemplate CreateChangeRoutineByTemplate", err)
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
			log.Println("ERROR: POST /usetemplate Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})

	// PUT /usetemplate 			// body: {id: "", templ_id: "", name: "", desc: "", interval:0, parameter: {<<param_name>>: <<param_value>>}}
	router.PUT("/usetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /usetemplate GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := UpdateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /usetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := state.UpdateChangeRoutineByTemplate(jwt, msg)
		if err != nil {
			log.Println("ERROR: PUT /usetemplate UpdateChangeRoutineByTemplate", err)
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
			log.Println("ERROR: PUT /usetemplate Marshal", err)
			http.Error(resp, err.Error(), 500)
		} else {
			fmt.Fprint(resp, string(b))
		}
	})
	return router
}
