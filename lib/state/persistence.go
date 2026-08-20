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

package state

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/SENERGY-Platform/moses/lib/config"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsoncodec"
	"go.mongodb.org/mongo-driver/bson/mgocompat"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type PersistenceInterface interface {
	PersistWorld(world World) (err error)
	PersistTemplate(templ RoutineTemplate) error
	LoadWorlds() (map[string]*World, error)
	GetTemplate(id string) (templ RoutineTemplate, err error)
	GetTemplates() (templ []RoutineTemplate, err error)
	DeleteWorld(id string) error
	DeleteTemplate(id string) error
}

// ErrNotFound is returned by GetTemplate() if no template with the requested id exists.
// it replaces the previously used mgo.ErrNotFound and is checked with errors.Is() by the callers.
// it is deliberately its own sentinel and not mongo.ErrNoDocuments, so that a not-found of some
// future other operation is not mistaken for "template does not exist".
var ErrNotFound = errors.New("not found")

// mongoTimeout is used for single document database operations
const mongoTimeout = 10 * time.Second

// mongoLoadTimeout is used for the collection scans, which have to transfer every world on startup.
// mgo had no operation deadline at all (only a per-round-trip socket timeout), so a slow but healthy
// database must not turn into a failed startup here.
const mongoLoadTimeout = 2 * time.Minute

// mongoMaxPoolSize mirrors mgo's DefaultConnectionPoolLimit. the driver default of 100 would be a
// regression: PersistWorld() is called synchronously by every js state.set(), so change routines
// would block on connection checkout and lose state changes once the deadline hits.
const mongoMaxPoolSize = 4096

// mongoRegistry keeps the bson (de)serialisation compatible with the previously used mgo driver:
// bson int32 is decoded to int, bson arrays to []interface{} and nil maps/slices are encoded as
// empty documents/arrays. this matters for the free form map[string]interface{} states, which are
// passed to the js vm and marshalled to json. note that nested documents inside those states are
// decoded to plain map[string]interface{} (not bson.M), same as mgo did.
var mongoRegistry *bsoncodec.Registry = mgocompat.Registry

type MongoPersistence struct {
	client                 *mongo.Client
	worldCollectionName    string
	templateCollectionName string
	tableName              string
}

func NewMongoPersistence(config config.Config) (result MongoPersistence, err error) {
	result.worldCollectionName = config.WorldCollectionName
	result.templateCollectionName = config.TemplateCollectionName
	result.tableName = config.MongoTable
	//mgo.Dial() accepted urls without a scheme, ApplyURI() rejects them
	mongoUrl := config.MongoUrl
	if !strings.Contains(mongoUrl, "://") {
		mongoUrl = "mongodb://" + mongoUrl
	}
	//server selection may legitimately take longer than a single operation, for example
	//while a replica set is electing a new primary
	ctx, cancel := context.WithTimeout(context.Background(), mongoLoadTimeout)
	defer cancel()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoUrl).SetRegistry(mongoRegistry).SetMaxPoolSize(mongoMaxPoolSize))
	if err != nil {
		log.Println("ERROR: NewMongoPersistence()::mongo.Connect()", err)
		return result, err
	}
	err = client.Ping(ctx, nil)
	if err != nil {
		log.Println("ERROR: NewMongoPersistence()::client.Ping()", err)
		disconnect(client)
		return result, err
	}
	result.client = client
	return result, nil
}

func (this *MongoPersistence) Close() {
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
		log.Println("ERROR: unable to disconnect from mongodb", err)
	}
}

func newContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), mongoTimeout)
}

func newLoadContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), mongoLoadTimeout)
}

func (this MongoPersistence) getWorldCollection() *mongo.Collection {
	return this.client.Database(this.tableName).Collection(this.worldCollectionName)
}

func (this MongoPersistence) getTemplateCollection() *mongo.Collection {
	return this.client.Database(this.tableName).Collection(this.templateCollectionName)
}

func (this MongoPersistence) PersistWorld(world World) (err error) {
	world.CleanStates()
	ctx, cancel := newContext()
	defer cancel()
	//not logged here: every caller already logs the error with a stack trace
	_, err = this.getWorldCollection().ReplaceOne(ctx, bson.M{"id": world.Id}, world, options.Replace().SetUpsert(true))
	return
}

func (this MongoPersistence) PersistTemplate(templ RoutineTemplate) (err error) {
	ctx, cancel := newContext()
	defer cancel()
	_, err = this.getTemplateCollection().ReplaceOne(ctx, bson.M{"id": templ.Id}, templ, options.Replace().SetUpsert(true))
	if err != nil {
		log.Println("ERROR: PersistTemplate()", err)
	}
	return
}

// GetTemplate returns ErrNotFound if no template with the given id exists
func (this MongoPersistence) GetTemplate(id string) (templ RoutineTemplate, err error) {
	ctx, cancel := newContext()
	defer cancel()
	err = this.getTemplateCollection().FindOne(ctx, bson.M{"id": id}).Decode(&templ)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return templ, ErrNotFound
	}
	if err != nil {
		log.Println("ERROR: GetTemplate()", err)
	}
	return
}

func (this MongoPersistence) GetTemplates() (templ []RoutineTemplate, err error) {
	templ = []RoutineTemplate{}
	ctx, cancel := newLoadContext()
	defer cancel()
	cursor, err := this.getTemplateCollection().Find(ctx, bson.M{})
	if err != nil {
		log.Println("ERROR: GetTemplates()", err)
		return templ, err
	}
	err = cursor.All(ctx, &templ)
	if err != nil {
		log.Println("ERROR: GetTemplates()::cursor.All()", err)
	}
	return
}

func (this MongoPersistence) LoadWorlds() (result map[string]*World, err error) {
	result = map[string]*World{}
	ctx, cancel := newLoadContext()
	defer cancel()
	cursor, err := this.getWorldCollection().Find(ctx, bson.M{})
	if err != nil {
		log.Println("ERROR: LoadWorlds()", err)
		return result, err
	}
	defer cursor.Close(context.Background())
	for cursor.Next(ctx) {
		//decoded one by one on purpose: mgo silently zeroed fields whose stored bson type did not
		//match the go type, the current driver returns an error instead. decoding the whole cursor
		//at once would let a single unreadable world document prevent the service from starting
		//with any world at all.
		world := World{}
		err = cursor.Decode(&world)
		if err != nil {
			log.Println("ERROR: LoadWorlds() skipping undecodable world document:", err)
			continue
		}
		world.mux = &sync.Mutex{}
		world.CleanStates()
		result[world.Id] = &world
	}
	err = cursor.Err()
	if err != nil {
		log.Println("ERROR: LoadWorlds()::cursor.Err()", err)
		return result, err
	}
	return result, nil
}

func (this MongoPersistence) DeleteWorld(id string) (err error) {
	ctx, cancel := newContext()
	defer cancel()
	_, err = this.getWorldCollection().DeleteMany(ctx, bson.M{"id": id})
	if err != nil {
		log.Println("ERROR: DeleteWorld()", err)
	}
	return
}

func (this MongoPersistence) DeleteTemplate(id string) (err error) {
	ctx, cancel := newContext()
	defer cancel()
	_, err = this.getTemplateCollection().DeleteMany(ctx, bson.M{"id": id})
	if err != nil {
		log.Println("ERROR: DeleteTemplate()", err)
	}
	return
}
