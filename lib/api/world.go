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
	endpoints = append(endpoints, WorldEndpoints)
}

func WorldEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {
	// PUTS only work on current level. sublevel will be preserved ( for example, put on room wont change devices of the room or change what devices the room has )
	// empty on list == []; not nil
	// states are managed by crud of parent entity

	// C	= 	POST
	// R	= 	GET
	// U 	= 	PUT
	// D	= 	DELETE

	// GET /worlds
	router.GET("/worlds", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		result, err := states.ReadWorlds(token)
		if err != nil {
			util.Logger.Error("unable to read worlds", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read worlds")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// PUT /world
	router.PUT("/world", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateWorldRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateWorld(token, msg)
		if err != nil {
			util.Logger.Error("unable to update world", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update world")
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

	// POST /world
	router.POST("/world", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateWorldRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, err := states.CreateWorld(token, msg)
		if err != nil {
			util.Logger.Error("unable to create world", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create world")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /world/:id
	router.GET("/world/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, access, exists, err := states.ReadWorld(token, id)
		if err != nil {
			util.Logger.Error("unable to read world", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read world")
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

	// DELETE /world/:id
	router.DELETE("/world/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		access, exists, err := states.DeleteWorld(token, id)
		if err != nil {
			util.Logger.Error("unable to delete world", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete world")
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
