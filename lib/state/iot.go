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

package state

import (
	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/util"
	permClient "github.com/SENERGY-Platform/permissions-v2/pkg/client"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/google/uuid"
	"net/url"
)

func (this *StateRepo) GetIotDeviceType(token sc_jwt.Token, id string) (dt model.DeviceType, err error) {
	err = deviceManagerGetJson(token.Jwt(), this.Config.DeviceManagerUrl+"/device-types/"+url.PathEscape(id), &dt)
	if err != nil {
		util.Logger.Error("unable to get device type", attributes.ErrorKey, err, "id", id)
	}
	return
}

func (this *StateRepo) GetIotDeviceTypes(token sc_jwt.Token) (result []model.DeviceType, err error) {
	err = deviceManagerGetJson(token.Jwt(), this.Config.DeviceManagerUrl+"/device-types", &result)
	if err != nil {
		util.Logger.Error("unable to list device types", attributes.ErrorKey, err)
	}
	return
}

func (this *StateRepo) GetIotDeviceTypesIds(token sc_jwt.Token) (result []string, err error) {
	steps := 1000
	limit := 0
	offset := 0
	temp := []string{}
	c := permClient.New(this.Config.PermissionsV2Url)
	for len(temp) == limit {
		limit = steps
		temp, err, _ = c.AdminListResourceIds(permClient.InternalAdminToken, "device-types", permClient.ListOptions{
			Limit:  int64(limit),
			Offset: int64(offset),
		})
		if err != nil {
			return result, err
		}
		result = append(result, temp...)
		offset = offset + limit
	}
	return
}

func (this *StateRepo) GetMosesDeviceTypesIds(token sc_jwt.Token) (result []string, err error) {
	steps := 1000
	limit := 0
	offset := 0
	temp := []models.DeviceType{}
	c := deviceRepo.NewClient(this.Config.DeviceRepoUrl, nil)
	for len(temp) == limit {
		limit = steps
		temp, _, err, _ = c.ListDeviceTypesV3(permClient.InternalAdminToken, deviceRepo.DeviceTypeListOptions{
			Limit:       int64(limit),
			Offset:      int64(offset),
			ProtocolIds: []string{this.MosesProtocolId},
			SortBy:      "name.asc",
		})
		if err != nil {
			return result, err
		}
		for _, element := range temp {
			result = append(result, element.Id)
		}
		offset = offset + limit
	}
	return
}

func (this *StateRepo) GenerateExternalDevice(token sc_jwt.Token, request CreateDeviceByTypeRequest) (device model.Device, err error) {
	deviceInp := model.Device{Name: request.Name, DeviceTypeId: request.DeviceTypeId, LocalId: uuid.NewString()}
	err = deviceManagerPostJson(token.Jwt(), this.Config.DeviceManagerUrl+"/devices", deviceInp, &device)
	if err != nil {
		util.Logger.Error("unable to create device in device repository", attributes.ErrorKey, err, "device_type_id", request.DeviceTypeId, "name", request.Name)
	}
	return
}

func (this *StateRepo) DeleteExternalDevice(token sc_jwt.Token, id string) (err error) {
	if id != "" {
		err = deviceManagerDelete(token.Jwt(), this.Config.DeviceManagerUrl+"/devices/"+url.PathEscape(id))
	}
	return
}

func (this *StateRepo) GetProtocolList(handler string) (result []models.Protocol, err error) {
	token, err := this.Connector.Security().Access()
	if err != nil {
		return result, err
	}
	result, err, _ = deviceRepo.NewClient(this.Config.DeviceRepoUrl, nil).ListProtocols(string(token), 1000, 0, "name.asc")
	return result, err
}

func (this *StateRepo) EnsureProtocol(handler string, segments []model.ProtocolSegment) (protocolId string, err error) {
	protocols, err := this.GetProtocolList(handler)
	if err != nil {
		return protocolId, err
	}
	if len(protocols) == 1 {
		return protocols[0].Id, err
	}
	if len(protocols) > 1 {
		util.Logger.Warn("found multiple existing moses protocols")
		return protocols[0].Id, err
	}
	protocol, err := this.CreateProtocol(handler, segments)
	if err != nil {
		return protocolId, err
	}
	protocolId = protocol.Id
	return protocolId, err
}

func (this *StateRepo) CreateProtocol(handler string, segments []model.ProtocolSegment) (protocol model.Protocol, err error) {
	token, err := this.Connector.Security().Access()
	if err != nil {
		return protocol, err
	}
	err = token.PostJSON(this.Config.DeviceManagerUrl+"/protocols", model.Protocol{
		Name:             handler,
		Handler:          handler,
		ProtocolSegments: segments,
	}, &protocol)
	if err != nil {
		util.Logger.Error("unable to create protocol", attributes.ErrorKey, err, "handler", handler)
	}
	return
}
