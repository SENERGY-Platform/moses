/*
 * Copyright 2026 InfAI (CC SES)
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
	"strings"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/util"
	"github.com/gin-gonic/gin"
)

func init() {
	catalogEndpoints = append(catalogEndpoints, CatalogEndpoints)
}

// CatalogEndpoints serve what an editor needs to offer a choice instead of
// asking for an identifier: the device types an asset can be built from, and
// the platform device such an asset publishes through.
func CatalogEndpoints(config config.Config, catalog DeviceCatalog, router gin.IRouter) {
	for _, route := range []func(DeviceCatalog) (string, string, gin.HandlerFunc){
		listDeviceTypesH,
		postDeviceH,
		deleteDeviceH,
	} {
		method, path, handler := route(catalog)
		router.Handle(method, path, handler)
	}
}

// @Summary List the device types an asset can be built from
// @Description Every device type that publishes through this service's protocol, with its services: the id, name, direction and characteristic of each, which is everything the channels of an asset need. Offering this list is what lets an editor ask for a device type instead of an id.
// @Tags Catalog
// @Produce json
// @Security Bearer
// @Success 200 {array} devices.DeviceType
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /device-types [get]
func listDeviceTypesH(catalog DeviceCatalog) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/device-types", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		result, err := catalog.DeviceTypes(token.Jwt())
		if err != nil {
			util.Logger.Error("unable to list device types", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to list device types")
			return
		}
		gc.JSON(http.StatusOK, result)
	}
}

// CreateDeviceRequest asks for a platform device of a given type.
type CreateDeviceRequest struct {
	DeviceTypeId string `json:"device_type_id"`
	Name         string `json:"name"`
}

// @Summary Create the platform device of a simulated asset
// @Description Registers a device of the given type and returns its id, which becomes the asset's external_ref. It is created with the caller's own token, so it belongs to whoever built the asset.
// @Tags Catalog
// @Accept json
// @Produce json
// @Security Bearer
// @Param device body CreateDeviceRequest true "device type and name"
// @Success 201 {object} devices.Device
// @Failure 400 {string} string "the body is unreadable or incomplete"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /devices [post]
func postDeviceH(catalog DeviceCatalog) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/devices", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		request := CreateDeviceRequest{}
		if err := gc.ShouldBindJSON(&request); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body: %s", err.Error())
			return
		}
		if strings.TrimSpace(request.DeviceTypeId) == "" || strings.TrimSpace(request.Name) == "" {
			gc.String(http.StatusBadRequest, "device_type_id and name must be set")
			return
		}
		device, err := catalog.CreateDevice(gc.Request.Context(), token.Jwt(), request.DeviceTypeId, request.Name)
		if err != nil {
			util.Logger.Error("unable to create the device", attributes.ErrorKey, err, "device_type_id", request.DeviceTypeId)
			gc.String(http.StatusInternalServerError, "unable to create the device: %s", err.Error())
			return
		}
		gc.JSON(http.StatusCreated, device)
	}
}

// @Summary Delete the platform device of a simulated asset
// @Description Removing an asset should not leave its device behind. Deleting one that does not exist is not an error.
// @Tags Catalog
// @Security Bearer
// @Param id path string true "device id"
// @Success 204 {string} string "deleted"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /devices/{id} [delete]
func deleteDeviceH(catalog DeviceCatalog) (string, string, gin.HandlerFunc) {
	return http.MethodDelete, "/devices/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		if err := catalog.DeleteDevice(gc.Request.Context(), token.Jwt(), gc.Param("id")); err != nil {
			util.Logger.Error("unable to delete the device", attributes.ErrorKey, err, "device", gc.Param("id"))
			gc.String(http.StatusInternalServerError, "unable to delete the device: %s", err.Error())
			return
		}
		gc.Status(http.StatusNoContent)
	}
}
