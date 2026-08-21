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
	endpoints = append(endpoints, DeviceEndpoints)
}

func DeviceEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {
	// POST /device/bydevicetype
	router.POST("/device/bydevicetype", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateDeviceByTypeRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, worldAndRoomExists, err := states.CreateDeviceByType(token, msg)
		if err != nil {
			util.Logger.Error("unable to create device by type", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create device by type")
			return
		}
		if !access {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		if !worldAndRoomExists {
			gc.String(http.StatusNotFound, "unknown world or room id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// PUT /device
	router.PUT("/device", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateDeviceRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateDevice(token, msg)
		if err != nil {
			util.Logger.Error("unable to update device", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update device")
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

	// POST /device
	router.POST("/device", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateDeviceRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, worldAndRoomExists, err := states.CreateDevice(token, msg)
		if err != nil {
			util.Logger.Error("unable to create device", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create device")
			return
		}
		if !access {
			gc.String(http.StatusUnauthorized, "access denied")
			return
		}
		if !worldAndRoomExists {
			gc.String(http.StatusNotFound, "unknown world or room id")
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /device/:id
	router.GET("/device/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, access, exists, err := states.ReadDevice(token, id)
		if err != nil {
			util.Logger.Error("unable to read device", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read device")
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

	// DELETE /device/:id
	router.DELETE("/device/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		_, access, exists, err := states.DeleteDevice(token, id)
		if err != nil {
			util.Logger.Error("unable to delete device", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete device")
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
