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

package repo

import (
	"errors"
	"net"
	"strings"
	"testing"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/mongoclient"
)

const startupTestPassword = "pw-must-not-appear-9b2d"

// unreachableStartupConfig points at a port that was free a moment ago, with a
// short server selection timeout so the failing startup check returns quickly.
func unreachableStartupConfig(t *testing.T) config.Config {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		MongoUrl:                  sb_config_types.Secret("mongodb://" + addr + "/?directConnection=true&serverSelectionTimeoutMS=200"),
		MongoUser:                 "moses",
		MongoPassword:             startupTestPassword,
		MongoAuthSource:           "admin",
		MongoDatabase:             "moses",
		WorldCollectionName:       "worlds",
		TemplateCollectionName:    "templates",
		EnvironmentCollectionName: "environments",
		StateCollectionName:       "environment_states",
		DatasetCollectionName:     "datasets",
	}
}

// The server is unreachable, so only a check made before connecting can return
// the specific error.
func TestStartupRejectsAnInvalidMongoConfigBeforeConnecting(t *testing.T) {
	emptyDatabase := unreachableStartupConfig(t)
	emptyDatabase.MongoDatabase = ""
	userWithoutPassword := unreachableStartupConfig(t)
	userWithoutPassword.MongoPassword = ""
	cases := map[string]struct {
		conf config.Config
		want error
	}{
		"empty database":        {emptyDatabase, mongoclient.ErrEmptyDatabase},
		"user without password": {userWithoutPassword, mongoclient.ErrMissingPassword},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store, err := NewMongo(c.conf)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if store != nil {
				t.Error("expected no connection after a rejected config")
			}
		})
	}
}

func TestStartupFailsWhenTheStartupCheckFails(t *testing.T) {
	store, err := NewMongo(unreachableStartupConfig(t))
	if err == nil {
		t.Fatal("expected an error against an unreachable server")
	}
	if !strings.HasPrefix(err.Error(), "mongo startup check failed: ") {
		t.Errorf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), startupTestPassword) {
		t.Errorf("error leaks the password: %v", err)
	}
	if store != nil {
		t.Error("expected no connection after a failed startup check")
	}
}
