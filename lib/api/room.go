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
	endpoints = append(endpoints, RoomEndpoints)
}

func RoomEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {
	// PUT /room
	router.PUT("/room", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /room GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.UpdateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /room Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.UpdateRoom(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /room GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateRoomRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /room Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, worldExists, err := states.CreateRoom(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /room/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := states.ReadRoom(jwt, id)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /room/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := states.DeleteRoom(jwt, id)
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
}
