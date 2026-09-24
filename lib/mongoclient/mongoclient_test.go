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

package mongoclient

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const testPassword = "pw-must-not-appear-5c1e"

const replicaSetURL = "mongodb://mongo-0.mongo:27017,mongo-1.mongo:27017/?replicaSet=rs0&readPreference=primary"

func TestOptionsSetsAuthWhenUserGiven(t *testing.T) {
	opts, err := Options(config.Config{
		MongoUrl:        "mongodb://localhost:27017",
		MongoUser:       "moses",
		MongoPassword:   testPassword,
		MongoAuthSource: "admin",
		MongoDatabase:   "moses",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &options.Credential{Username: "moses", Password: testPassword, AuthSource: "admin"}
	if !reflect.DeepEqual(opts.Auth, want) {
		t.Errorf("auth = %+v, want %+v", opts.Auth, want)
	}
}

func TestOptionsSetsNoAuthWithoutUser(t *testing.T) {
	// a password alone must not switch authentication on
	opts, err := Options(config.Config{
		MongoUrl:        "mongodb://localhost:27017",
		MongoPassword:   testPassword,
		MongoAuthSource: "admin",
		MongoDatabase:   "moses",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Auth != nil {
		t.Errorf("auth = %+v, want nil", opts.Auth)
	}
}

func TestOptionsConfiguredCredentialsReplaceThoseOfTheURL(t *testing.T) {
	opts, err := Options(config.Config{
		MongoUrl:        "mongodb://old:oldpw@localhost:27017/?authSource=other&authMechanism=SCRAM-SHA-1",
		MongoUser:       "moses",
		MongoPassword:   testPassword,
		MongoAuthSource: "admin",
		MongoDatabase:   "moses",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := &options.Credential{Username: "moses", Password: testPassword, AuthSource: "admin"}
	if !reflect.DeepEqual(opts.Auth, want) {
		t.Errorf("auth = %+v, want %+v", opts.Auth, want)
	}
}

func TestOptionsPassesTheURLUnchanged(t *testing.T) {
	opts, err := Options(config.Config{MongoUrl: replicaSetURL, MongoDatabase: "moses"})
	if err != nil {
		t.Fatal(err)
	}
	if got := opts.GetURI(); got != replicaSetURL {
		t.Errorf("uri = %q, want %q", got, replicaSetURL)
	}
	if want := []string{"mongo-0.mongo:27017", "mongo-1.mongo:27017"}; !reflect.DeepEqual(opts.Hosts, want) {
		t.Errorf("hosts = %v, want %v", opts.Hosts, want)
	}
	if opts.ReplicaSet == nil || *opts.ReplicaSet != "rs0" {
		t.Errorf("replica set = %v, want rs0", opts.ReplicaSet)
	}
}

func TestOptionsRejects(t *testing.T) {
	cases := map[string]struct {
		conf config.Config
		want error
	}{
		"empty database":        {config.Config{MongoUrl: "mongodb://localhost:27017", MongoUser: "moses", MongoPassword: testPassword}, ErrEmptyDatabase},
		"user without password": {config.Config{MongoUrl: "mongodb://localhost:27017", MongoUser: "moses", MongoDatabase: "moses"}, ErrMissingPassword},
		// the scheme is not added any more, a url without one is a config error
		"url without scheme": {config.Config{MongoUrl: "localhost:27017", MongoUser: "moses", MongoPassword: testPassword, MongoDatabase: "moses"}, nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			opts, err := Options(c.conf)
			if err == nil {
				t.Fatal("expected an error")
			}
			if opts != nil {
				t.Error("expected no options on error")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Errorf("error leaks the password: %v", err)
			}
		})
	}
}

func TestTheStartupCheckTimeoutIsTenSeconds(t *testing.T) {
	if startupCheckTimeout != 10*time.Second {
		t.Errorf("startupCheckTimeout = %v", startupCheckTimeout)
	}
}

// unreachableURL names a port that was free a moment ago, so no server answers.
func unreachableURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "mongodb://" + addr + "/?directConnection=true"
}

func TestConnectFailingStartupCheckLeavesNothingConnected(t *testing.T) {
	opts, err := Options(config.Config{
		MongoUrl:        sb_config_types.Secret(unreachableURL(t)),
		MongoUser:       "moses",
		MongoPassword:   testPassword,
		MongoAuthSource: "admin",
		MongoDatabase:   "moses",
	})
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	opts.SetServerMonitor(&event.ServerMonitor{TopologyClosed: func(*event.TopologyClosedEvent) { closed.Store(true) }})

	start := time.Now()
	client, err := connect(context.Background(), opts, "moses", 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error")
	}
	if client != nil {
		t.Error("expected no client on failure")
	}
	if !strings.HasPrefix(err.Error(), "mongo startup check failed: ") {
		t.Errorf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error leaks the password: %v", err)
	}
	if !closed.Load() {
		t.Error("the client of the failed check was not disconnected")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("connect took %v, the check timeout was not applied", elapsed)
	}
}

// startTestMongo starts a mongodb without access control for this one test.
func startTestMongo(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "mongo:7.0",
			ExposedPorts: []string{"27017/tcp"},
			WaitingFor: wait.ForAll(
				wait.ForLog("Waiting for connections"),
				wait.ForListeningPort("27017/tcp"),
			),
			Tmpfs: map[string]string{"/data/db": "rw"},
		},
		Started: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	port, err := container.MappedPort(ctx, "27017/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return "mongodb://localhost:" + port.Port()
}

// The service database does not exist yet on a fresh replica set, and the check
// must not mistake that for a failure.
func TestConnectWithoutAccessControlOnADatabaseThatDoesNotExistYet(t *testing.T) {
	url := startTestMongo(t)
	opts, err := Options(config.Config{MongoUrl: sb_config_types.Secret(url), MongoAuthSource: "admin", MongoDatabase: "moses"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := Connect(context.Background(), opts, "moses")
	if err != nil {
		t.Fatalf("expected the startup check to pass, got %v", err)
	}
	t.Cleanup(func() { _ = client.Disconnect(context.Background()) })
	names, err := client.Database("moses").ListCollectionNames(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("expected a usable client, got %v", err)
	}
	if len(names) != 0 {
		t.Errorf("expected an empty database, got %v", names)
	}
}
