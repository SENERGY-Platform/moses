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

package api

import (
	"encoding/json"
	"fmt"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/jwt"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/julienschmidt/httprouter"
	"log"
	"net/http"
)

func init() {
	endpoints = append(endpoints, OtherEndpoints)
}

func OtherEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {
	router.POST("/run/service/:id", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		id := params.ByName("id")
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /run/service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		_, access, exists, err := states.ReadService(jwt, id)
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
		result, err := states.RunService(id, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /device/bydevicetype GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateDeviceByTypeRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: /device/bydevicetype Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := states.CreateDeviceByType(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /devicetypes GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := states.GetMosesDeviceTypesIds(jwt)
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
}
