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

// defaultShareCollectionName is used when the config names none.
const defaultShareCollectionName = "environment_shares"

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
	shareCollectionName       string
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
		shareCollectionName:       config.ShareCollectionName,
	}
	if result.shareCollectionName == "" {
		//defaulted rather than demanded: the field is younger than the
		//deployments, and a mounted config from before it must not fail startup
		result.shareCollectionName = defaultShareCollectionName
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
	if err != nil {
		return err
	}
	_, err = this.shareCollection().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "environment_id", Value: 1}},
		Options: options.Index().SetName("share_environment_id_index").SetUnique(true),
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

func (this *Mongo) shareCollection() *mongo.Collection {
	return this.client.Database(this.database).Collection(this.shareCollectionName)
}

// Put writes without a concurrency check and creates the document if it is not
// there yet.
func (this *Mongo) Put(ctx context.Context, env domain.Environment) (int64, error) {
	if strings.TrimSpace(env.Id) == "" {
		return 0, fmt.Errorf("%w: environment", ErrMissingId)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	version, err := replaceAndIncrement(ctx, this.environmentCollection(), bson.M{"id": env.Id}, env, true)
	if err != nil {
		util.Logger.Error("unable to put environment", attributes.ErrorKey, err, "id", env.Id)
		return 0, err
	}
	return version, nil
}

// PutIfVersion writes only while the stored document still carries
// expectedVersion. The comparison is the filter of the write itself, so it is
// decided by the database on the document, not by this process on a copy it read
// a moment ago.
//
// It never creates: a caller that carries a version read a document, and if that
// document is gone, recreating it under the version of a deleted one would be
// the opposite of what the caller asked for.
func (this *Mongo) PutIfVersion(ctx context.Context, env domain.Environment, expectedVersion int64) (int64, error) {
	if strings.TrimSpace(env.Id) == "" {
		return 0, fmt.Errorf("%w: environment", ErrMissingId)
	}
	if expectedVersion < 1 {
		//not treated as "no check": a stored document never carries a version
		//below 1, so this can only be a caller mistake, and turning it into an
		//unchecked write would remove the protection it asked for
		return 0, fmt.Errorf("%w: expected version must be 1 or higher, got %d", ErrVersionConflict, expectedVersion)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	version, err := replaceAndIncrement(ctx, this.environmentCollection(),
		bson.M{"id": env.Id, "version": expectedVersion}, env, false)
	if errors.Is(err, mongo.ErrNoDocuments) {
		//nothing matched, so either the version moved on or the document is
		//gone. Which of the two is worth saying, and the caller cannot find out
		//without a read of its own
		conflict := &VersionConflictError{Id: env.Id, Expected: expectedVersion}
		conflict.Stored, conflict.Gone, conflict.StoredUnknown = this.versionOf(env.Id)
		return 0, conflict
	}
	if err != nil {
		util.Logger.Error("unable to put environment", attributes.ErrorKey, err, "id", env.Id)
		return 0, err
	}
	return version, nil
}

// versionOf reads the version of a stored document, for a conflict message only.
// It reports the three answers apart, because "it is gone", "it is at version 3"
// and "this could not be read" send a caller to three different places.
//
// It takes a context of its own rather than the caller's: the caller's may
// already be spent on the write that was just refused, and a message that then
// claims the document disappeared would be worse than no message.
func (this *Mongo) versionOf(id string) (version int64, gone bool, unknown bool) {
	ctx, cancel := newContext()
	defer cancel()
	stored := storedVersion{}
	err := this.environmentCollection().FindOne(ctx, bson.M{"id": id},
		options.FindOne().SetProjection(bson.M{"version": 1})).Decode(&stored)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, true, false
	}
	if err != nil {
		util.Logger.Warn("unable to read the stored version for a conflict message", attributes.ErrorKey, err, "id", id)
		return 0, false, true
	}
	return stored.Version, false, false
}

// storedVersion is the projection both the write and the conflict message read
// back: the whole document would be transferred for one number otherwise.
type storedVersion struct {
	Version int64 `bson:"version"`
}

// replaceAndIncrement replaces the whole document and sets its version to the
// stored version plus one, in the database, as one operation.
//
// The increment deliberately does not happen here: reading the version, adding
// one and writing the result back would be a read-modify-write, and two writers
// doing it at once would both write the same number - after which a
// compare-and-swap could accept a document that was written over. The pipeline
// below lets the server read, add and write in the same step on the same
// document, so every successful write gets a version of its own.
//
// The filter decides what is written and what is refused: {id} writes whatever
// is stored, {id, version} writes only that one version and matches nothing
// otherwise, which is the compare-and-swap.
func replaceAndIncrement(ctx context.Context, collection *mongo.Collection, filter bson.M, env domain.Environment, upsert bool) (int64, error) {
	opts := options.FindOneAndUpdate().
		SetUpsert(upsert).
		SetReturnDocument(options.After).
		SetProjection(bson.M{"version": 1})
	stored := storedVersion{}
	err := collection.FindOneAndUpdate(ctx, filter, versionedReplacement(env), opts).Decode(&stored)
	if err != nil && upsert && mongo.IsDuplicateKeyError(err) {
		//same race the plain upsert had: two writers find no document, both try
		//to insert, and the unique index rejects one of them. The retry finds
		//the document the other one inserted and replaces it. Documented by
		//mongodb as the remedy for exactly this case.
		err = collection.FindOneAndUpdate(ctx, filter, versionedReplacement(env), opts).Decode(&stored)
	}
	if err != nil {
		return 0, err
	}
	return stored.Version, nil
}

// versionedReplacement is an update pipeline rather than a replacement document,
// because a replacement cannot refer to what is stored and the new version has
// to be the stored one plus one.
//
// $literal is not decoration: the document is data, and without it the
// aggregation would read every string starting with a "$" as a field path - a
// zone named "$hall" or, far more likely, a line of script code.
func versionedReplacement(env domain.Environment) mongo.Pipeline {
	//the version the caller handed in is overwritten by the second operand:
	//$mergeObjects lets the later document win, and what a client sent is never
	//what gets stored
	return mongo.Pipeline{{{Key: "$replaceWith", Value: bson.M{"$mergeObjects": bson.A{
		bson.M{"$literal": env},
		//int64 literals on purpose: they make the stored version a bson int64
		//whatever the arithmetic does, which is the type the document decodes into
		bson.M{"version": bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$version", int64(0)}}, int64(1)}}},
	}}}}}
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
		return err
	}
	//the share set goes with the definition, or an id that is used again would
	//come back shared with the accounts of the environment that is gone
	_, err = this.shareCollection().DeleteMany(ctx, bson.M{"environment_id": id})
	if err != nil {
		util.Logger.Error("unable to delete the share set of an environment", attributes.ErrorKey, err, "id", id)
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
