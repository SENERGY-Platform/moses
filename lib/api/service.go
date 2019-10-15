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
	endpoints = append(endpoints, ServiceEndpoints)
}

func ServiceEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {

	// PUT /service
	router.PUT("/service", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /service GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.UpdateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /service Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.UpdateService(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /service GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateServiceRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /service Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldAndRoomExists, err := states.CreateService(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := states.ReadService(jwt, id)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /service/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := states.DeleteService(jwt, id)
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

}
