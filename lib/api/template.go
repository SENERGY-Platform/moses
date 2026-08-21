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
	"net/http"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/gin-gonic/gin"
)

func init() {
	endpoints = append(endpoints, TemplateEndpoints)
}

func TemplateEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {

	// PUT /routinetemplate					// body: {id: "", name: "", desc: "", templ:""}
	router.PUT("/routinetemplate", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		if !token.IsAdmin() {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		msg := state.UpdateTemplateRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, exists, err := states.UpdateTemplate(token, msg)
		if err != nil {
			util.Logger.Error("unable to update template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update template")
			return
		}
		if !exists {
			gc.String(http.StatusNotFound, "unknown id")
			return
		}

		gc.JSON(http.StatusOK, result)
	})

	// POST /routinetemplate				// body: {name: "", desc: "", templ:""}
	router.POST("/routinetemplate", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		if !token.IsAdmin() {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		msg := state.CreateTemplateRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, err := states.CreateTemplate(token, msg)
		if err != nil {
			util.Logger.Error("unable to create template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create template")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /routinetemplate/:id			// body: {id: "", name: "", desc: "", templ:"", parameter: [""]}
	router.GET("/routinetemplate/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, exists, err := states.ReadTemplate(token, id)
		if err != nil {
			util.Logger.Error("unable to read template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read template")
			return
		}
		if !exists {
			gc.String(http.StatusNotFound, "unknown id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /routinetemplates			// contains default templates created by moses
	router.GET("/routinetemplates", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		result, err := states.ReadTemplates(token)
		if err != nil {
			util.Logger.Error("unable to read templates", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read templates")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// DELETE /routinetemplate/:id
	router.DELETE("/routinetemplate/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		if !token.IsAdmin() {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		id := gc.Param("id")
		err := states.DeleteTemplate(token, id)
		if err != nil {
			util.Logger.Error("unable to delete template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete template")
			return
		}

		gc.String(http.StatusOK, "ok")
	})

	// POST /usetemplate 			// body: {ref_type:"workd|room|device", ref_id: "", templ_id: "", name: "", desc: "", parameter: {<<param_name>>: <<param_value>>}}
	router.POST("/usetemplate", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateChangeRoutineByTemplateRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.CreateChangeRoutineByTemplate(token, msg)
		if err != nil {
			util.Logger.Error("unable to create change routine by template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create change routine by template")
			return
		}
		if !access {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		if !exists {
			gc.String(http.StatusNotFound, "unknown world, room or device id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// PUT /usetemplate 			// body: {id: "", templ_id: "", name: "", desc: "", interval:0, parameter: {<<param_name>>: <<param_value>>}}
	router.PUT("/usetemplate", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateChangeRoutineByTemplateRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateChangeRoutineByTemplate(token, msg)
		if err != nil {
			util.Logger.Error("unable to update change routine by template", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update change routine by template")
			return
		}
		if !access {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		if !exists {
			gc.String(http.StatusNotFound, "unknown id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})
}
