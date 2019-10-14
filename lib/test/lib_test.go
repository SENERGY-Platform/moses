package test

import (
	"github.com/SENERGY-Platform/moses/lib"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	"github.com/SENERGY-Platform/platform-connector-lib/model"
	"log"
	"testing"
	"time"
)

func TestStartup(t *testing.T) {
	defaultConfig, err := lib.LoadConfigLocation("../../config.json")
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

	log.Println("check moses protocol init")
	protocols := []model.Protocol{}
	err = helper.AdminGet(t, config.DeviceRepoUrl+"/protocols", &protocols)
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

	log.Println("done")
}
