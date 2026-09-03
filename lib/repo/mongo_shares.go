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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// MongoShares is the Shares half of the store, on the connection of the Mongo it
// came from. A type of its own for the same reason MongoStates is one: the two
// Delete methods mean different things.
type MongoShares struct {
	store *Mongo
}

var _ Shares = &MongoShares{}

// Shares returns the share store on the same connection.
func (this *Mongo) Shares() *MongoShares {
	return &MongoShares{store: this}
}

// Load returns an empty set for an environment nobody shared, so a caller can
// read the lists without checking for nil first.
func (this *MongoShares) Load(ctx context.Context, environmentId string) (result ShareSet, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err = this.store.shareCollection().FindOne(ctx, bson.M{"environment_id": environmentId}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return emptyShareSet(environmentId), nil
	}
	if err != nil {
		util.Logger.Error("unable to load the share set", attributes.ErrorKey, err, "environment_id", environmentId)
		return emptyShareSet(environmentId), err
	}
	if result.Users == nil {
		result.Users = []string{}
	}
	if result.Groups == nil {
		result.Groups = []string{}
	}
	return result, nil
}

func emptyShareSet(environmentId string) ShareSet {
	return ShareSet{EnvironmentId: environmentId, Users: []string{}, Groups: []string{}}
}

// Save is a compare-and-swap, and the comparison is the filter of the write
// itself, so it is decided by the database rather than by this process on a copy
// it read a moment ago. Two shares of one environment arriving together would
// otherwise each store their own set and leave the rights of the other standing
// on the devices, remembered nowhere.
//
// The collection is versioned from its first document, so a stored set without a
// version cannot occur.
func (this *MongoShares) Save(ctx context.Context, shares ShareSet) (int64, error) {
	if strings.TrimSpace(shares.EnvironmentId) == "" {
		return 0, fmt.Errorf("%w: share set", ErrMissingId)
	}
	if shares.Version < 0 {
		return 0, fmt.Errorf("%w: expected version must not be negative, got %d", ErrVersionConflict, shares.Version)
	}
	//set by the store, as documented on the field
	shares.UpdatedAtUnix = time.Now().Unix()
	expected := shares.Version
	shares.Version = expected + 1

	ctx, cancel := withTimeout(ctx)
	defer cancel()
	if expected == 0 {
		//nothing was read, so nothing may be there: the unique index is what
		//decides between two callers that both found none
		_, err := this.store.shareCollection().InsertOne(ctx, shares)
		if mongo.IsDuplicateKeyError(err) {
			return 0, this.conflict(shares.EnvironmentId, expected)
		}
		if err != nil {
			util.Logger.Error("unable to save the share set", attributes.ErrorKey, err, "environment_id", shares.EnvironmentId)
			return 0, err
		}
		return shares.Version, nil
	}
	result, err := this.store.shareCollection().ReplaceOne(ctx,
		bson.M{"environment_id": shares.EnvironmentId, "version": expected}, shares)
	if err != nil {
		util.Logger.Error("unable to save the share set", attributes.ErrorKey, err, "environment_id", shares.EnvironmentId)
		return 0, err
	}
	if result.MatchedCount == 0 {
		return 0, this.conflict(shares.EnvironmentId, expected)
	}
	return shares.Version, nil
}

// conflict reads the stored version back for the message: "it moved to 3" and
// "it is gone" send a caller to two different places.
func (this *MongoShares) conflict(environmentId string, expected int64) error {
	result := &VersionConflictError{Id: environmentId, Expected: expected}
	ctx, cancel := newContext()
	defer cancel()
	stored := ShareSet{}
	err := this.store.shareCollection().FindOne(ctx, bson.M{"environment_id": environmentId}).Decode(&stored)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		result.Gone = true
	case err != nil:
		result.StoredUnknown = true
	default:
		result.Stored = stored.Version
	}
	return result
}

func (this *MongoShares) Delete(ctx context.Context, environmentId string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := this.store.shareCollection().DeleteMany(ctx, bson.M{"environment_id": environmentId})
	if err != nil {
		util.Logger.Error("unable to delete the share set", attributes.ErrorKey, err, "environment_id", environmentId)
	}
	return err
}
