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
	endpoints = append(endpoints, TemplateEndpoints)
}

func TemplateEndpoints(config config.Config, states *state.StateRepo, router *httprouter.Router) {

	// PUT /routinetemplate					// body: {id: "", name: "", desc: "", templ:""}
	router.PUT("/routinetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
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
		msg := state.UpdateTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /routinetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, exists, err := states.UpdateTemplate(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
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
		msg := state.CreateTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: jsondecode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := states.CreateTemplate(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /routinetemplate/:id GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		id := params.ByName("id")
		result, exists, err := states.ReadTemplate(jwt, id)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: GET /routinetemplates GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, err := states.ReadTemplates(jwt)
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
		jwt, err := jwt.GetJwt(request)
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
		err = states.DeleteTemplate(jwt, id)
		if err != nil {
			log.Println("ERROR: DELETE /routinetemplate/:id DeleteTemplate", err)
			http.Error(resp, err.Error(), 500)
			return
		}

		fmt.Fprint(resp, "ok")
	})

	// POST /usetemplate 			// body: {ref_type:"workd|room|device", ref_id: "", templ_id: "", name: "", desc: "", parameter: {<<param_name>>: <<param_value>>}}
	router.POST("/usetemplate", func(resp http.ResponseWriter, request *http.Request, params httprouter.Params) {
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: POST /usetemplate GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.CreateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: POST /usetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.CreateChangeRoutineByTemplate(jwt, msg)
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
		jwt, err := jwt.GetJwt(request)
		if err != nil {
			log.Println("ERROR: PUT /usetemplate GetJwt", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		msg := state.UpdateChangeRoutineByTemplateRequest{}
		err = json.NewDecoder(request.Body).Decode(&msg)
		if err != nil {
			log.Println("ERROR: PUT /usetemplate Decode", err)
			http.Error(resp, err.Error(), 400)
			return
		}
		result, access, exists, err := states.UpdateChangeRoutineByTemplate(jwt, msg)
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
}
