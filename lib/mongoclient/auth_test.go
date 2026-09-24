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

package mongoclient_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	sb_config_types "github.com/SENERGY-Platform/go-service-base/config-hdl/types"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/state"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// TestStoresAuthenticate needs a throwaway server with access control;
// MONGO_AUTH_TEST_USER and MONGO_AUTH_TEST_PASSWORD are its root credentials,
// used to create and remove the test users. Both stores of the service are
// started, since each opens its own connection.
func TestStoresAuthenticate(t *testing.T) {
	url, rootUser, rootPassword := os.Getenv("MONGO_AUTH_TEST_URL"), os.Getenv("MONGO_AUTH_TEST_USER"), os.Getenv("MONGO_AUTH_TEST_PASSWORD")
	if testing.Short() || url == "" || rootUser == "" || rootPassword == "" {
		t.Skip("needs MONGO_AUTH_TEST_URL, MONGO_AUTH_TEST_USER and MONGO_AUTH_TEST_PASSWORD, not in -short")
	}
	ctx := context.Background()
	root, err := mongo.Connect(ctx, options.Client().ApplyURI(url).SetAuth(options.Credential{Username: rootUser, Password: rootPassword, AuthSource: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Disconnect(ctx) })

	suffix := randomHex(t)
	testDB, otherDB := "moses_auth_test_"+suffix, "moses_auth_other_"+suffix
	svcUser, svcPassword := "moses-test-"+suffix, randomHex(t)
	otherUser, otherPassword := "moses-other-"+suffix, randomHex(t)
	createUser(t, root, svcUser, svcPassword, testDB)
	createUser(t, root, otherUser, otherPassword, otherDB)

	cases := []struct {
		name, user, password string
		wantErr              bool
	}{
		{"correct credentials", svcUser, svcPassword, false},
		{"no credentials", "", "", true},
		{"user of another database", otherUser, otherPassword, true},
		{"wrong password", svcUser, svcPassword + "-wrong", true},
	}
	for _, c := range cases {
		conf := config.Config{
			MongoUrl:                  sb_config_types.Secret(url),
			MongoUser:                 c.user,
			MongoPassword:             sb_config_types.Secret(c.password),
			MongoAuthSource:           "admin",
			MongoDatabase:             testDB,
			WorldCollectionName:       "worlds",
			TemplateCollectionName:    "templates",
			EnvironmentCollectionName: "environments",
			StateCollectionName:       "environment_states",
			DatasetCollectionName:     "datasets",
		}
		starts := map[string]func() error{
			"repo": func() error {
				store, err := repo.NewMongo(conf)
				if err == nil {
					store.Close()
				}
				return err
			},
			"state": func() error {
				store, err := state.NewMongoPersistence(conf)
				if err == nil {
					store.Close()
				}
				return err
			},
		}
		for storeName, start := range starts {
			t.Run(c.name+"/"+storeName, func(t *testing.T) {
				err := start()
				if (err != nil) != c.wantErr {
					t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
				}
				if err == nil {
					return
				}
				if !strings.HasPrefix(err.Error(), "mongo startup check failed: ") {
					t.Errorf("unexpected error: %v", err)
				}
				for _, pw := range []string{svcPassword, otherPassword, rootPassword} {
					if strings.Contains(err.Error(), pw) {
						t.Error("error text contains a password")
					}
				}
			})
		}
	}
}

// createUser registers the removal first, so a creation that fails halfway
// leaves nothing behind either.
func createUser(t *testing.T, root *mongo.Client, user, password, db string) {
	t.Helper()
	admin := root.Database("admin")
	t.Cleanup(func() {
		err := admin.RunCommand(context.Background(), bson.D{{Key: "dropUser", Value: user}}).Err()
		var commandErr mongo.CommandError
		if err != nil && !(errors.As(err, &commandErr) && commandErr.Name == "UserNotFound") {
			t.Errorf("drop user: %v", err)
		}
		if err = root.Database(db).Drop(context.Background()); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	cmd := bson.D{
		{Key: "createUser", Value: user},
		{Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: db}}}},
	}
	if err := admin.RunCommand(context.Background(), cmd).Err(); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
