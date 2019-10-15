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
	endpoints = append(endpoints, WorldEndpoints)
}

func WorldEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {
	// PUTS only work on current level. sublevel will be preserved ( for example, put on room wont change devices of the room or change what devices the room has )
	// empty on list == []; not nil
	// states are managed by crud of parent entity

	// C	= 	POST
	// R	= 	GET
	// U 	= 	PUT
	// D	= 	DELETE

	// GET /worlds
	router.GET("/worlds", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET/worlds GetJwt()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := states.ReadWorlds(jwt)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT/world GetJwt()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.UpdateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT/world Decode()", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.UpdateWorld(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /world GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateWorldRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /world Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := states.CreateWorld(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /world/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, access, exists, err := states.ReadWorld(jwt, id)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: DELETE /world/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		access, exists, err := states.DeleteWorld(jwt, id)
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
}
