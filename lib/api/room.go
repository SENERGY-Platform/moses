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
	endpoints = append(endpoints, RoomEndpoints)
}

func RoomEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {
	// PUT /room
	router.PUT("/room", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateRoomRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateRoom(token, msg)
		if err != nil {
			util.Logger.Error("unable to update room", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update room")
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

	// POST /room
	router.POST("/room", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateRoomRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, worldExists, err := states.CreateRoom(token, msg)
		if err != nil {
			util.Logger.Error("unable to create room", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create room")
			return
		}
		if !access {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		if !worldExists {
			gc.String(http.StatusNotFound, "unknown world id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /room/:id
	router.GET("/room/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, access, exists, err := states.ReadRoom(token, id)
		if err != nil {
			util.Logger.Error("unable to read room", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read room")
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

	// DELETE /room/:id
	router.DELETE("/room/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		_, access, exists, err := states.DeleteRoom(token, id)
		if err != nil {
			util.Logger.Error("unable to delete room", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete room")
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
