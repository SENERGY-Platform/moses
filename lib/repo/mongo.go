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
	"sort"
	"strings"
	"time"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/mgocompat"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/gridfs"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrMissingId is returned instead of writing a document without an id. The
// upsert filter is the id, so an empty one would either create a second
// nameless document or collide with the unique index on the next write, and both
// are harder to diagnose than a rejected call.
var ErrMissingId = errors.New("id must not be empty")

// mongoTimeout is used for single document database operations
const mongoTimeout = 10 * time.Second

// mongoLoadTimeout is used for the collection scans, which have to transfer
// every environment on startup. A slow but healthy database must not turn into a
// failed startup here.
const mongoLoadTimeout = 2 * time.Minute

// mongoMaxPoolSize mirrors lib/state: a state write happens on every tick of
// every channel, so the driver default of 100 would make those writes queue on
// connection checkout and start losing state changes once the deadline hits.
const mongoMaxPoolSize = 4096

// mongoRegistry keeps the bson (de)serialisation compatible with the previously
// used mgo driver, exactly like lib/state does. This is not cosmetic: the free
// form map[string]interface{} of Environment.Context, Zone/Asset.InitialStates
// and RuntimeState pass through the js vm and the json marshalling of the api,
// and both see the concrete types the registry produces. With the driver default,
// a stored array comes back as primitive.A rather than []interface{} and a small
// number as int32 rather than int, so every type switch in the runtime would have
// to know which of the two stores a value came from.
var mongoRegistry *bsoncodec.Registry = mgocompat.Registry

// Mongo implements Environments on one mongodb connection.
//
// The runtime states live in the same connection but behind a second type: both
// interfaces declare Delete(context.Context, string) error, with different
// meanings - Environments.Delete removes the definition and its state, while
// States.Delete removes only the state. One method cannot be both, so States()
// hands out a view that implements the second interface.
type Mongo struct {
	client                    *mongo.Client
	database                  string
	environmentCollectionName string
	stateCollectionName       string
	datasetCollectionName     string
	datasetBucket             *gridfs.Bucket
}

// MongoStates is the States half of the store. It shares the connection of the
// Mongo it came from and holds no state of its own.
type MongoStates struct {
	store *Mongo
}

var _ Environments = &Mongo{}
var _ States = &MongoStates{}

// States returns the runtime state store on the same connection.
func (this *Mongo) States() *MongoStates {
	return &MongoStates{store: this}
}

// NewMongo connects, checks the connection and ensures the indexes exist.
func NewMongo(config config.Config) (result *Mongo, err error) {
	result = &Mongo{
		database:                  config.MongoTable,
		environmentCollectionName: config.EnvironmentCollectionName,
		stateCollectionName:       config.StateCollectionName,
		datasetCollectionName:     config.DatasetCollectionName,
	}
	if result.environmentCollectionName == "" || result.stateCollectionName == "" || result.datasetCollectionName == "" {
		return nil, errors.New("environment_collection_name, state_collection_name and dataset_collection_name must be configured")
	}
	//the legacy config allowed urls without a scheme, ApplyURI() rejects them
	mongoUrl := config.MongoUrl.Value()
	if !strings.Contains(mongoUrl, "://") {
		mongoUrl = "mongodb://" + mongoUrl
	}
	//server selection may legitimately take longer than a single operation, for
	//example while a replica set is electing a new primary
	ctx, cancel := newLoadContext()
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoUrl).SetRegistry(mongoRegistry).SetMaxPoolSize(mongoMaxPoolSize))
	if err != nil {
		util.Logger.Error("unable to connect to mongodb", attributes.ErrorKey, err)
		return nil, err
	}
	err = client.Ping(ctx, nil)
	if err != nil {
		util.Logger.Error("unable to reach mongodb", attributes.ErrorKey, err)
		disconnect(client)
		return nil, err
	}
	result.client = client
	result.datasetBucket, err = gridfs.NewBucket(client.Database(result.database),
		options.GridFSBucket().SetName(result.datasetCollectionName+"_content"))
	if err != nil {
		disconnect(client)
		return nil, err
	}
	err = result.ensureIndexes(ctx)
	if err != nil {
		util.Logger.Error("unable to ensure indexes", attributes.ErrorKey, err)
		disconnect(client)
		return nil, err
	}
	return result, nil
}

// ensureIndexes makes the id of an environment and the environment_id of a state
// unique in the database and not only by convention. Both are the filter of an
// upsert, and a duplicate would make Get() and Save() pick an arbitrary one of
// the copies.
func (this *Mongo) ensureIndexes(ctx context.Context) error {
	_, err := this.environmentCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetName("environment_id_index").SetUnique(true),
	})
	if err != nil {
		return err
	}
	_, err = this.stateCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "environment_id", Value: 1}},
		Options: options.Index().SetName("state_environment_id_index").SetUnique(true),
	})
	if err != nil {
		return err
	}
	_, err = this.datasetCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "id", Value: 1}},
		Options: options.Index().SetName("dataset_id_index").SetUnique(true),
	})
	return err
}

func (this *Mongo) Close() {
	if this.client == nil {
		return
	}
	disconnect(this.client)
}

func disconnect(client *mongo.Client) {
	ctx, cancel := newContext()
	defer cancel()
	err := client.Disconnect(ctx)
	if err != nil {
		util.Logger.Error("unable to disconnect from mongodb", attributes.ErrorKey, err)
	}
}

func newContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), mongoTimeout)
}

func newLoadContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), mongoLoadTimeout)
}

// withTimeout bounds one operation without ignoring what the caller asked for:
// if the caller's context expires earlier or is cancelled, that still wins.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, mongoTimeout)
}

func withLoadTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, mongoLoadTimeout)
}

func (this *Mongo) datasetCollection() *mongo.Collection {
	return this.client.Database(this.database).Collection(this.datasetCollectionName)
}

func (this *Mongo) environmentCollection() *mongo.Collection {
	return this.client.Database(this.database).Collection(this.environmentCollectionName)
}

func (this *Mongo) stateCollection() *mongo.Collection {
	return this.client.Database(this.database).Collection(this.stateCollectionName)
}

func (this *Mongo) Put(ctx context.Context, env domain.Environment) error {
	if strings.TrimSpace(env.Id) == "" {
		return fmt.Errorf("%w: environment", ErrMissingId)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := upsert(ctx, this.environmentCollection(), bson.M{"id": env.Id}, env)
	if err != nil {
		util.Logger.Error("unable to put environment", attributes.ErrorKey, err, "id", env.Id)
	}
	return err
}

// upsert replaces one document and retries once on a duplicate key error.
//
// An upsert is not atomic against a concurrent insert of the same id: when two
// writers race, one of them can find no document, try to insert, and be rejected
// by the unique index. Mongodb documents exactly this case and retrying as the
// remedy - the second attempt finds the document the other writer inserted and
// replaces it. Without the retry, two api calls or two simulation ticks arriving
// at the same time would make one of them fail for no reason the caller can act
// on.
func upsert(ctx context.Context, collection *mongo.Collection, filter bson.M, document interface{}) error {
	_, err := collection.ReplaceOne(ctx, filter, document, options.Replace().SetUpsert(true))
	if err != nil && mongo.IsDuplicateKeyError(err) {
		_, err = collection.ReplaceOne(ctx, filter, document, options.Replace().SetUpsert(true))
	}
	return err
}

// Get returns an error wrapping ErrNotFound if no environment with this id
// exists, so that a caller can tell "does not exist" from "database is down"
// with errors.Is().
func (this *Mongo) Get(ctx context.Context, id string) (result domain.Environment, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err = this.environmentCollection().FindOne(ctx, bson.M{"id": id}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return domain.Environment{}, fmt.Errorf("%w: %v", ErrNotFound, id)
	}
	if err != nil {
		util.Logger.Error("unable to get environment", attributes.ErrorKey, err, "id", id)
		return domain.Environment{}, err
	}
	return result, nil
}

func (this *Mongo) ListByOwner(ctx context.Context, owner string) (result []domain.Environment, err error) {
	result = []domain.Environment{}
	ctx, cancel := withLoadTimeout(ctx)
	defer cancel()
	cursor, err := this.environmentCollection().Find(ctx, bson.M{"owner": owner})
	if err != nil {
		util.Logger.Error("unable to list environments by owner", attributes.ErrorKey, err, "owner", owner)
		return result, err
	}
	defer cursor.Close(context.Background())
	for cursor.Next(ctx) {
		//decoded one by one for the same reason as in All()
		env := domain.Environment{}
		err = cursor.Decode(&env)
		if err != nil {
			util.Logger.Error("skipping undecodable environment document", attributes.ErrorKey, err)
			continue
		}
		result = append(result, env)
	}
	err = cursor.Err()
	if err != nil {
		util.Logger.Error("unable to list environments by owner", attributes.ErrorKey, err, "owner", owner)
		return result, err
	}
	//sorted here and not by the database on purpose: a list shown to a human is
	//expected in case insensitive order, which a plain mongodb sort does not give
	//(it would put "Zeta" before "alpha"). the id breaks ties, so that two
	//environments with the same name keep a stable order between calls.
	sort.SliceStable(result, func(a, b int) bool {
		nameA, nameB := strings.ToLower(result[a].Name), strings.ToLower(result[b].Name)
		if nameA != nameB {
			return nameA < nameB
		}
		return result[a].Id < result[b].Id
	})
	return result, nil
}

func (this *Mongo) All(ctx context.Context) (result []domain.Environment, err error) {
	result = []domain.Environment{}
	ctx, cancel := withLoadTimeout(ctx)
	defer cancel()
	cursor, err := this.environmentCollection().Find(ctx, bson.M{})
	if err != nil {
		util.Logger.Error("unable to list environments", attributes.ErrorKey, err)
		return result, err
	}
	defer cursor.Close(context.Background())
	for cursor.Next(ctx) {
		//decoded one by one on purpose: the driver returns an error when a stored
		//bson type does not match the go type, and decoding the whole cursor at
		//once would let a single unreadable document keep the service from
		//starting with any environment at all
		env := domain.Environment{}
		err = cursor.Decode(&env)
		if err != nil {
			util.Logger.Error("skipping undecodable environment document", attributes.ErrorKey, err)
			continue
		}
		result = append(result, env)
	}
	err = cursor.Err()
	if err != nil {
		util.Logger.Error("unable to list environments", attributes.ErrorKey, err)
		return result, err
	}
	return result, nil
}

// Delete removes the definition first and the runtime state second. Standalone
// mongodb has no transaction to make both atomic, so the order is chosen for
// what a failure in between leaves behind: the definition is gone, which is what
// the caller asked for, and a repeated Delete cleans up the state that is left.
// The other order would leave an environment running on reset values.
func (this *Mongo) Delete(ctx context.Context, id string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := this.environmentCollection().DeleteMany(ctx, bson.M{"id": id})
	if err != nil {
		util.Logger.Error("unable to delete environment", attributes.ErrorKey, err, "id", id)
		return err
	}
	_, err = this.stateCollection().DeleteMany(ctx, bson.M{"environment_id": id})
	if err != nil {
		util.Logger.Error("unable to delete environment state", attributes.ErrorKey, err, "id", id)
	}
	return err
}

// Load returns a state with initialised maps when nothing is stored yet, so that
// a caller can write into it without checking for nil first.
func (this *MongoStates) Load(ctx context.Context, environmentId string) (result RuntimeState, err error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err = this.store.stateCollection().FindOne(ctx, bson.M{"environment_id": environmentId}).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return emptyRuntimeState(environmentId), nil
	}
	if err != nil {
		util.Logger.Error("unable to load runtime state", attributes.ErrorKey, err, "environment_id", environmentId)
		return emptyRuntimeState(environmentId), err
	}
	if result.Context == nil {
		result.Context = map[string]interface{}{}
	}
	if result.Zones == nil {
		result.Zones = map[string]map[string]interface{}{}
	}
	if result.Assets == nil {
		result.Assets = map[string]map[string]interface{}{}
	}
	return result, nil
}

func emptyRuntimeState(environmentId string) RuntimeState {
	return RuntimeState{
		EnvironmentId: environmentId,
		Context:       map[string]interface{}{},
		Zones:         map[string]map[string]interface{}{},
		Assets:        map[string]map[string]interface{}{},
	}
}

func (this *MongoStates) Save(ctx context.Context, state RuntimeState) error {
	if strings.TrimSpace(state.EnvironmentId) == "" {
		return fmt.Errorf("%w: runtime state", ErrMissingId)
	}
	//set by the store, as documented on the field. the wall clock is correct here:
	//this is a timestamp that outlives the process, not a duration.
	state.UpdatedAtUnix = time.Now().Unix()
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	err := upsert(ctx, this.store.stateCollection(), bson.M{"environment_id": state.EnvironmentId}, state)
	if err != nil {
		util.Logger.Error("unable to save runtime state", attributes.ErrorKey, err, "environment_id", state.EnvironmentId)
	}
	return err
}

// Delete removes the state of one environment, but not its definition: an
// environment whose state is deleted starts again from its initial states.
func (this *MongoStates) Delete(ctx context.Context, environmentId string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := this.store.stateCollection().DeleteMany(ctx, bson.M{"environment_id": environmentId})
	if err != nil {
		util.Logger.Error("unable to delete runtime state", attributes.ErrorKey, err, "environment_id", environmentId)
	}
	return err
}
