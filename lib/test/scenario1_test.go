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
	"github.com/SENERGY-Platform/platform-connector-lib/kafka"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	uuid "github.com/satori/go.uuid"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestScenario1(t *testing.T) {
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

	log.Println("wait for protocol creation")
	time.Sleep(5 * time.Second)

	protocol := model.Protocol{}
	deviceType := model.DeviceType{}
	worldId, roomId := "", ""
	device := state.DeviceMsg{}

	t.Run("get protocol", func(t *testing.T) {
		protocol = getTestMosesProtocol(t, config)
	})

	t.Run("create device-type", func(t *testing.T) {
		deviceType = createTestDeviceType(t, config, protocol)
	})

	log.Println("wait for device-type creation")
	time.Sleep(5 * time.Second)

	t.Run("create world and room", func(t *testing.T) {
		worldId, roomId = createTestWorldAndRoom(t, config)
	})

	t.Run("create moses device", func(t *testing.T) {
		device = createMosesDevice(t, config, worldId, roomId, deviceType)
		log.Println("wait for device creation")
		time.Sleep(5 * time.Second)
		checkDevice(t, config, device)
	})

	t.Run("try command", func(t *testing.T) {
		tryCommandToDevice(t, config, protocol, deviceType, device)
	})

}

func tryCommandToDevice(t *testing.T, config config.Config, protocol model.Protocol, deviceType model.DeviceType, deviceMsg state.DeviceMsg) {
	service := model.Service{}
	for _, s := range deviceType.Services {
		if s.LocalId == "sepl_get" {
			service = s
			break
		}
	}
	err := kafka.InitTopic(config.ZookeeperUrl, model.ServiceIdToTopic(service.Id))
	if err != nil {
		t.Fatal(err)
	}
	mux := sync.Mutex{}
	responses := []model.ProtocolMsg{}
	consumer, err := kafka.NewConsumer(config.ZookeeperUrl, "testing_"+uuid.NewV4().String(), config.KafkaResponseTopic, func(topic string, msg []byte, time time.Time) error {
		mux.Lock()
		defer mux.Unlock()
		resp := model.ProtocolMsg{}
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			t.Fatal(err)
			return err
		}
		responses = append(responses, resp)
		return nil
	}, func(err error, consumer *kafka.Consumer) {
		t.Fatal(err)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Stop()

	producer, err := kafka.PrepareProducer(config.ZookeeperUrl, config.SyncKafka, config.SyncKafkaIdempotent)
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(model.ProtocolMsg{
		Request:  model.ProtocolRequest{},
		Response: model.ProtocolResponse{},
		TaskInfo: model.TaskInfo{},
		Metadata: model.Metadata{
			Device: model.Device{
				Id:           deviceMsg.ExternalRef,
				LocalId:      deviceMsg.Id,
				DeviceTypeId: deviceType.Id,
			},
			Service:  service,
			Protocol: protocol,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = producer.Produce(config.Protocol, string(request))
	if err != nil {
		t.Fatal(err)
	}
	log.Println("wait for command handling")
	time.Sleep(5 * time.Second)

	mux.Lock()
	defer mux.Unlock()
	if len(responses) != 1 {
		t.Fatal("unexpected response count", responses)
	}
	if responses[0].Response.Output["metrics"] != "{\"level\":0,\"title\":\"\",\"updateTime\":0}" {
		t.Fatal("unexpected response msg", responses[0].Response.Output)
	}
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
