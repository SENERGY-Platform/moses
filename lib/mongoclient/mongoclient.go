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

// Package mongoclient builds and checks the mongodb connection that lib/state
// and lib/repo each open from the same config.
package mongoclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	ErrEmptyDatabase   = errors.New("mongo database name must not be empty")
	ErrMissingPassword = errors.New("mongo password must not be empty when a mongo user is set")
)

// startupCheckTimeout bounds the authenticated check after connecting.
const startupCheckTimeout = 10 * time.Second

// disconnectTimeout bounds the disconnect after a failed check.
const disconnectTimeout = 10 * time.Second

// Options turns the config into client options without touching the network.
// The credentials are applied after the URI, so they replace any user, password,
// authSource and authMechanism given in MONGO_URL.
func Options(conf config.Config) (*options.ClientOptions, error) {
	if conf.MongoDatabase == "" {
		return nil, ErrEmptyDatabase
	}
	opts := options.Client().ApplyURI(conf.MongoUrl.Value())
	if conf.MongoUser != "" {
		if conf.MongoPassword.Value() == "" {
			return nil, ErrMissingPassword
		}
		opts.SetAuth(options.Credential{
			Username:   conf.MongoUser,
			Password:   conf.MongoPassword.Value(),
			AuthSource: conf.MongoAuthSource,
		})
	}
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("invalid mongo client options: %w", err)
	}
	return opts, nil
}

// Connect connects and lists the collections of database, because Connect is
// lazy and ping answers without authentication. On failure the client is
// disconnected and nil is returned.
func Connect(ctx context.Context, opts *options.ClientOptions, database string) (*mongo.Client, error) {
	return connect(ctx, opts, database, startupCheckTimeout)
}

func connect(ctx context.Context, opts *options.ClientOptions, database string, timeout time.Duration) (*mongo.Client, error) {
	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err = client.Database(database).ListCollectionNames(checkCtx, bson.D{},
		options.ListCollections().SetNameOnly(true).SetAuthorizedCollections(true))
	if err != nil {
		//a fresh context: the one of the check may be the reason it failed
		disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), disconnectTimeout)
		defer cancelDisconnect()
		_ = client.Disconnect(disconnectCtx)
		return nil, fmt.Errorf("mongo startup check failed: %w", err)
	}
	return client, nil
}
