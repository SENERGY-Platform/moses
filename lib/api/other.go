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
	endpoints = append(endpoints, OtherEndpoints)
}

func OtherEndpoints(config config.Config, states *state.StateRepo, router gin.IRouter) {
	router.POST("/run/service/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")
		_, access, exists, err := states.ReadService(token, id)
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
		var msg interface{}
		err = gc.ShouldBindJSON(&msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		result, err := states.RunService(id, msg)
		if err != nil {
			gc.String(http.StatusBadRequest, "%s", err.Error())
			return
		}
		gc.JSON(http.StatusOK, result)
	})

	// GET /devicetypes
	// returns list of device type ids which use the moses protocol
	// to get DeviceType objects you can call the permsearch endpoint POST /ids/select/:resource_kind/:right ; /ids/select/:resource_kind/:right/:limit/:offset/:orderfeature/:direction or by requesting the iot repository
	router.GET("/devicetypes", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		result, err := states.GetMosesDeviceTypesIds(token)
		if err != nil {
			util.Logger.Error("unable to read device types", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read device types")
			return
		}
		gc.JSON(http.StatusOK, result)
	})
}
