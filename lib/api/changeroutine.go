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
	endpoints = append(endpoints, ChangeroutineEndpoints)
}

func ChangeroutineEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {

	// PUT /changeroutine					//{id:"", interval: 0, code:""}
	router.PUT("/changeroutine", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateChangeRoutineRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateChangeRoutine(token, msg)
		if err != nil {
			util.Logger.Error("unable to update change routine", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update change routine")
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

	// POST /changeroutine					//{ref_type:"workd|room|device", ref_id: "", interval: 0, code:""}
	router.POST("/changeroutine", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateChangeRoutineRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.CreateChangeRoutine(token, msg)
		if err != nil {
			util.Logger.Error("unable to create change routine", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create change routine")
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

	// GET /changeroutine/:id
	router.GET("/changeroutine/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, access, exists, err := states.ReadChangeRoutine(token, id)
		if err != nil {
			util.Logger.Error("unable to read change routine", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read change routine")
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

	// DELETE /changeroutine/:id
	router.DELETE("/changeroutine/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		_, access, exists, err := states.DeleteChangeRoutine(token, id)
		if err != nil {
			util.Logger.Error("unable to delete change routine", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete change routine")
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
		gc.String(http.StatusOK, "ok")
	})

}
