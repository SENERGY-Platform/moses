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
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"log"
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

	if protocols[0].ProtocolSegments[0].Name != "payload" {
		t.Fatal("unexpected protocol segment name", protocols[0].ProtocolSegments[0].Name, "payload")
	}

	return protocols[0]
}
