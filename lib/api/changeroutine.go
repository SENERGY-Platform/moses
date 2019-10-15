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
	endpoints = append(endpoints, ChangeroutineEndpoints)
}

func ChangeroutineEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {

	// PUT /changeroutine					//{id:"", interval: 0, code:""}
	router.PUT("/changeroutine", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /changeroutine GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.UpdateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /changeroutine Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.UpdateChangeRoutine(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /changeroutine GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateChangeRoutineRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /changeroutine Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.CreateChangeRoutine(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /changeroutine/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := states.ReadChangeRoutine(jwt, id)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /changeroutine/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		_, access, exists, err := states.DeleteChangeRoutine(jwt, id)
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

}
