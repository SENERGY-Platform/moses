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
	endpoints = append(endpoints, ServiceEndpoints)
}

func ServiceEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {

	// PUT /service
	router.PUT("/service", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.UpdateServiceRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, exists, err := states.UpdateService(token, msg)
		if err != nil {
			util.Logger.Error("unable to update service", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to update service")
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

	// POST /service
	router.POST("/service", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		msg := state.CreateServiceRequest{}
		err := gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, access, worldAndRoomExists, err := states.CreateService(token, msg)
		if err != nil {
			util.Logger.Error("unable to create service", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to create service")
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

	// GET /service/:id
	router.GET("/service/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		result, access, exists, err := states.ReadService(token, id)
		if err != nil {
			util.Logger.Error("unable to read service", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read service")
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

	// DELETE /service/:id
	router.DELETE("/service/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		_, access, exists, err := states.DeleteService(token, id)
		if err != nil {
			util.Logger.Error("unable to delete service", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete service")
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
