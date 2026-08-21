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
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/state"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/kafka"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"github.com/google/uuid"
	"io/ioutil"
	"log"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSensor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with docker containers in short mode")
	}
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultConfig, err := config.LoadConfigLocation("../../config.json")
	if err != nil {
		t.Error(err)
		return
	}

	log.Println("startup")
	config, err := server.New(ctx, wg, defaultConfig, "./server/keycloak-export.json")
	if err != nil {
		t.Error(err)
		return
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

	t.Run("try sensor", func(t *testing.T) {
		trySensorFromDevice(t, config, protocol, deviceType, device)
	})

	t.Run("try environment sensor", func(t *testing.T) {
		tryEnvironmentSensor(t, config, deviceType, worldId, roomId)
	})

}

// tryEnvironmentSensor is the new runtime end to end: an environment stored
// through the api has to publish for its asset exactly the way a legacy world
// published for its device, through the same connector onto the same topic.
//
// The platform device is created through the legacy flow on purpose. That is
// what a migrated environment references: the asset keeps the device id and the
// channel keeps the service id of the world it was converted from, and those two
// refs are what keeps the existing timeseries attached.
func tryEnvironmentSensor(t *testing.T, conf config.Config, deviceType model.DeviceType, worldId string, roomId string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	//a second device, and one whose legacy service interval stays 0: every event
	//that arrives for it therefore comes from the environment runtime and not
	//from the legacy change routines
	device := createMosesDevice(t, conf, worldId, roomId, deviceType)
	log.Println("wait for device creation")
	time.Sleep(5 * time.Second)
	checkDevice(t, conf, device)

	service := model.Service{}
	for _, s := range deviceType.Services {
		if s.LocalId == "sepl_get" {
			service = s
			break
		}
	}
	if service.Id == "" {
		t.Fatal("the test device type has no sepl_get service")
	}

	mux := sync.Mutex{}
	events := []model.Envelope{}
	err := kafka.NewConsumer(ctx, kafka.ConsumerConfig{
		KafkaUrl:       conf.KafkaUrl,
		GroupId:        "testing_" + uuid.NewString(),
		Topic:          model.ServiceIdToTopic(service.Id),
		MinBytes:       int(conf.KafkaConsumerMinBytes),
		MaxBytes:       int(conf.KafkaConsumerMaxBytes),
		MaxWait:        100 * time.Millisecond,
		TopicConfigMap: conf.KafkaTopicConfigs,
		InitTopic:      true,
	}, func(topic string, msg []byte, time time.Time) error {
		resp := model.Envelope{}
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			return err
		}
		//the topic carries the events of every device of this service, the first
		//test device included, so the envelopes are filtered by device
		if resp.DeviceId != device.ExternalRef {
			return nil
		}
		mux.Lock()
		defer mux.Unlock()
		events = append(events, resp)
		return nil
	}, func(err error) {
		//logged and not failed: this callback runs on the consumer's own
		//goroutine, which outlives the test function, and t.Error from there
		//panics instead of failing the test
		log.Println("ERROR: kafka consumer:", err)
	})
	if err != nil {
		t.Fatal(err)
	}

	//the channel script is a legacy script, unchanged: it sends the value of the
	//service's output content variable, which is exactly what the skeleton
	//generated for a legacy service does. The platform wraps it under the name of
	//that variable ("metrics"), and its CleanMsg drops every field the device
	//type does not declare - so a payload that repeats the variable name, or a
	//bare number, arrives with empty leaves. That is not a property of the new
	//runtime; the legacy runtime behaves the same, and the assertion below is the
	//same one the legacy test makes.
	environmentId := uuid.NewString()
	environment := domain.Environment{
		Name:    "test_environment",
		Type:    domain.IndustrialSite,
		Context: map[string]interface{}{},
		Zones: []domain.Zone{{
			Name:          "test_hall",
			Type:          domain.ZoneHall,
			InitialStates: map[string]interface{}{},
			Assets: []domain.Asset{{
				Name:           "test_machine",
				Kind:           domain.AssetMachine,
				ExternalRef:    device.ExternalRef,
				ExternalTypeId: device.ExternalTypeId,
				InitialStates:  map[string]interface{}{},
				Channels: []domain.Channel{{
					Name:            "sepl_get",
					Direction:       domain.Sensor,
					ExternalRef:     service.Id,
					IntervalSeconds: 1,
					Source: domain.Source{Kind: domain.SourceScript, Script: &domain.ScriptSource{
						Code: `moses.service.send({"level":7,"title":"env","updateTime":0});`,
					}},
				}},
			}},
		}},
	}
	stored := domain.Environment{}
	err = helper.AdminJwt.PutJSON("http://localhost:"+conf.ServerPort+"/environments/"+environmentId, environment, &stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Id != environmentId {
		t.Fatal("unexpected environment id", stored.Id)
	}

	if !waitFor(60*time.Second, func() bool {
		mux.Lock()
		defer mux.Unlock()
		return len(events) > 0
	}) {
		t.Fatal("no environment sensor events received within 60s")
	}

	mux.Lock()
	defer mux.Unlock()
	event := events[0]
	if event.ServiceId != service.Id {
		t.Fatal("unexpected envelope", event)
	}
	var expected interface{}
	err = json.Unmarshal([]byte(`{"metrics":{"level":7,"title":"env","updateTime":0}}`), &expected)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(event.Value, expected) {
		t.Fatal(event.Value, "\n\n!=\n\n", expected)
	}
}

func trySensorFromDevice(t *testing.T, config config.Config, protocol model.Protocol, deviceType model.DeviceType, device state.DeviceMsg) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Run("set sensor time", func(t *testing.T) {
		setSensorTime(t, config, device, 1)
	})
	service := model.Service{}
	for _, s := range deviceType.Services {
		if s.LocalId == "sepl_get" {
			service = s
			break
		}
	}
	mux := sync.Mutex{}
	events := []model.Envelope{}

	err := kafka.NewConsumer(ctx, kafka.ConsumerConfig{
		KafkaUrl:       config.KafkaUrl,
		GroupId:        "testing_" + uuid.NewString(),
		Topic:          model.ServiceIdToTopic(service.Id),
		MinBytes:       int(config.KafkaConsumerMinBytes),
		MaxBytes:       int(config.KafkaConsumerMaxBytes),
		MaxWait:        100 * time.Millisecond,
		TopicConfigMap: config.KafkaTopicConfigs,
		//create the topic before joining: a consumer group that joins before its topic exists
		//gets 0 partitions assigned and only recovers on the 1 minute partition watch interval
		InitTopic: true,
	}, func(topic string, msg []byte, time time.Time) error {
		mux.Lock()
		defer mux.Unlock()
		resp := model.Envelope{}
		err := json.Unmarshal(msg, &resp)
		if err != nil {
			t.Fatal(err)
			return err
		}
		events = append(events, resp)
		return nil
	}, func(err error) {
		t.Fatal(err)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !waitFor(60*time.Second, func() bool {
		mux.Lock()
		defer mux.Unlock()
		return len(events) > 0
	}) {
		t.Fatal("no sensor events received within 60s")
	}

	mux.Lock()
	defer mux.Unlock()
	event := events[0]
	if event.DeviceId != device.ExternalRef {
		t.Fatal("unexpected envelope", event)
	}
	if event.ServiceId != service.Id {
		t.Fatal("unexpected envelope", event)
	}

	var expected interface{}
	err = json.Unmarshal([]byte("{\"metrics\":{\"level\":0,\"title\":\"\",\"updateTime\":0}}"), &expected)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(event.Value, expected) {
		t.Fatal(event.Value, "\n\n!=\n\n", expected)
	}
}

func setSensorTime(t *testing.T, config config.Config, deviceMsg state.DeviceMsg, seconds int64) {
	//PUT /service UpdateServiceRequest
	service := state.Service{}
	for _, s := range deviceMsg.Services {
		if s.Name == "sepl_get" {
			service = s
			break
		}
	}

	client := http.Client{
		Timeout: 5 * time.Second,
	}
	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(state.UpdateServiceRequest{
		Id:             service.Id,
		Name:           service.Name,
		ExternalRef:    service.ExternalRef,
		Code:           service.Code,
		SensorInterval: seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PUT", "http://localhost:"+config.ServerPort+"/service", b)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
}
