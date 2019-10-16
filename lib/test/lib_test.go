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

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestStartup(t *testing.T) {
	defaultConfig, err := config.LoadConfigLocation("../../config.json")
	if err != nil {
		t.Fatal(err)
	}

	log.Println("startup")
	config, stop, err := server.New(defaultConfig, "./server/keycloak-export.json")
	defer stop()
	if err != nil {
		t.Fatal(err)
	}

	log.Println("wait")
	time.Sleep(5 * time.Second)

	t.Run("check moses protocol init", func(t *testing.T) {
		getTestMosesProtocol(t, config)
	})
}

func getTestMosesProtocol(t *testing.T, config config.Config) model.Protocol {
	protocols := []model.Protocol{}
	err := helper.AdminGet(t, config.DeviceRepoUrl+"/protocols", &protocols)
	if err != nil {
		t.Fatal(err)
	}

	if len(protocols) != 1 {
		t.Fatal("unexpected protocol count", protocols)
	}

	if protocols[0].Handler != config.Protocol {
		t.Fatal("unexpected protocol handler", protocols[0].Handler, config.Protocol)
	}

	if len(protocols[0].ProtocolSegments) != 1 {
		t.Fatal("unexpected segment count", protocols[0].ProtocolSegments)
	}

	if protocols[0].ProtocolSegments[0].Name != config.ProtocolSegmentName {
		t.Fatal("unexpected protocol segment name", protocols[0].ProtocolSegments[0].Name, config.ProtocolSegmentName)
	}

	return protocols[0]
}

func checkDevice(t *testing.T, config config.Config, device state.DeviceMsg) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	req, err := http.NewRequest("GET", config.DeviceManagerUrl+"/devices/"+url.PathEscape(device.ExternalRef), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", string(helper.AdminJwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		t.Fatal(err)
	}
	result := model.Device{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != device.Name {
		t.Fatal("unexpected result", result)
	}
}

func createTestDeviceType(t *testing.T, config config.Config, protocol model.Protocol) (result model.DeviceType) {
	err := helper.AdminJwt.PostJSON(config.DeviceManagerUrl+"/device-types", model.DeviceType{
		Name: "foo",
		Services: []model.Service{
			{
				Name:        "sepl_get",
				LocalId:     "sepl_get",
				Description: "sepl_get",
				ProtocolId:  protocol.Id,
				Outputs: []model.Content{
					{
						ProtocolSegmentId: protocol.ProtocolSegments[0].Id,
						Serialization:     "json",
						ContentVariable: model.ContentVariable{
							Name: "metrics",
							Type: model.Structure,
							SubContentVariables: []model.ContentVariable{
								{
									Name: "updateTime",
									Type: model.Integer,
								},
								{
									Name: "level",
									Type: model.Integer,
								},
								{
									Name: "title",
									Type: model.String,
								},
							},
						},
					},
				},
			},
			{
				Name:        "exact",
				LocalId:     "exact",
				Description: "exact",
				ProtocolId:  protocol.Id,
				Inputs: []model.Content{
					{
						ProtocolSegmentId: protocol.ProtocolSegments[0].Id,
						Serialization:     "json",
						ContentVariable: model.ContentVariable{
							Name: "metrics",
							Type: model.Structure,
							SubContentVariables: []model.ContentVariable{
								{
									Name: "level",
									Type: model.Integer,
								},
							},
						},
					},
				},
			},
		},
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Id == "" {
		t.Fatal("unexpected result", result)
	}
	return
}

func createTestWorldAndRoom(t *testing.T, config config.Config) (worldId string, roomId string) {
	worldId = createTestWorld(t, config)
	roomId = createTestRoom(t, config, worldId)
	return
}

func createTestWorld(t *testing.T, config config.Config) (worldId string) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(state.CreateWorldRequest{
		Name:   "test_world",
		States: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", "http://localhost:"+config.ServerPort+"/world", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", string(helper.AdminJwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		t.Fatal(err)
	}

	result := state.WorldMsg{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result.Id
}

func createTestRoom(t *testing.T, config config.Config, worldId string) (roomId string) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(state.CreateRoomRequest{
		World:  worldId,
		Name:   "test_room",
		States: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", "http://localhost:"+config.ServerPort+"/room", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", string(helper.AdminJwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		t.Fatal(err)
	}

	result := state.RoomResponse{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result.Room.Id
}

func createMosesDevice(t *testing.T, config config.Config, worldId string, roomId string, deviceType model.DeviceType) (device state.DeviceMsg) {
	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(state.CreateDeviceByTypeRequest{
		DeviceTypeId: deviceType.Id,
		Room:         roomId,
		Name:         "test_device",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", "http://localhost:"+config.ServerPort+"/device/bydevicetype", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	req.WithContext(ctx)
	req.Header.Set("Authorization", string(helper.AdminJwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		responseBody, _ := ioutil.ReadAll(resp.Body)
		err = errors.New(resp.Status + ": " + string(responseBody))
		t.Fatal(err)
	}

	result := state.DeviceResponse{}
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		t.Fatal(err)
	}
	return result.Device
}
