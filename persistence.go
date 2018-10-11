/*
 * Copyright 2018 SENERGY Team
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

package main

import (
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
)

type PersistenceInterface interface {
	PersistWorld(world World) (err error)
	PersistGraph(graph Graph) (err error)
	PersistTemplate(templ RoutineTemplate) error
	LoadWorlds() (map[string]*World, error)
	LoadGraphs() (map[string]*Graph, error)
	GetTemplate(id string) (templ RoutineTemplate, err error)
	GetTemplates() (templ []RoutineTemplate, err error)
	DeleteWorld(id string) error
	DeleteGraph(id string) error
	DeleteTemplate(id string) error
}

type MongoPersistence struct {
	session                *mgo.Session
	worldCollectionName    string
	graphCollectionName    string
	templateCollectionName string
	tableName              string
}

func NewMongoPersistence(config Config) (result MongoPersistence, err error) {
	result.worldCollectionName = config.WorldCollectionName
	result.graphCollectionName = config.GraphCollectionName
	result.templateCollectionName = config.TemplateCollectionName
	result.session, err = mgo.Dial(config.MongoUrl)
	if err == nil {
		result.session.SetMode(mgo.Monotonic, true)
	}
	return
}

func (this MongoPersistence) getWorldCollection() (session *mgo.Session, collection *mgo.Collection) {
	session = this.session.Copy()
	collection = session.DB(this.tableName).C(this.worldCollectionName)
	return
}

func (this MongoPersistence) getGraphCollection() (session *mgo.Session, collection *mgo.Collection) {
	session = this.session.Copy()
	collection = session.DB(this.tableName).C(this.graphCollectionName)
	return
}

func (this MongoPersistence) getTemplateCollection() (session *mgo.Session, collection *mgo.Collection) {
	session = this.session.Copy()
	collection = session.DB(this.tableName).C(this.templateCollectionName)
	return
}

func (this MongoPersistence) PersistWorld(world World) (err error) {
	session, collection := this.getWorldCollection()
	defer session.Close()
	_, err = collection.Upsert(bson.M{"id": world.Id}, world)
	return
}

func (this MongoPersistence) PersistGraph(graph Graph) (err error) {
	session, collection := this.getGraphCollection()
	defer session.Close()
	_, err = collection.Upsert(bson.M{"id": graph.Id}, graph)
	return
}

func (this MongoPersistence) PersistTemplate(templ RoutineTemplate) (err error) {
	session, collection := this.getTemplateCollection()
	defer session.Close()
	_, err = collection.Upsert(bson.M{"id": templ.Id}, templ)
	return
}

func (this MongoPersistence) GetTemplate(id string) (templ RoutineTemplate, err error) {
	session, collection := this.getTemplateCollection()
	defer session.Close()
	err = collection.Find(bson.M{"id": id}).One(&templ)
	return
}

func (this MongoPersistence) GetTemplates() (templ []RoutineTemplate, err error) {
	session, collection := this.getTemplateCollection()
	defer session.Close()
	err = collection.Find(nil).All(&templ)
	return
}

func (this MongoPersistence) LoadWorlds() (result map[string]*World, err error) {
	session, collection := this.getWorldCollection()
	defer session.Close()
	worlds := []World{}
	err = collection.Find(nil).All(&worlds)
	if err != nil {
		return result, err
	}
	for _, world := range worlds {
		result[world.Id] = &world
	}
	return
}

func (this MongoPersistence) LoadGraphs() (result map[string]*Graph, err error) {
	session, collection := this.getGraphCollection()
	defer session.Close()
	graphs := []Graph{}
	err = collection.Find(nil).All(&graphs)
	if err != nil {
		return result, err
	}
	for _, graph := range graphs {
		result[graph.Id] = &graph
	}
	return
}

func (this MongoPersistence) DeleteWorld(id string) (err error) {
	session, collection := this.getWorldCollection()
	defer session.Close()
	_, err = collection.RemoveAll(bson.M{"id": id})
	return
}

func (this MongoPersistence) DeleteGraph(id string) (err error) {
	session, collection := this.getGraphCollection()
	defer session.Close()
	_, err = collection.RemoveAll(bson.M{"id": id})
	return
}

func (this MongoPersistence) DeleteTemplate(id string) (err error) {
	session, collection := this.getTemplateCollection()
	defer session.Close()
	_, err = collection.RemoveAll(bson.M{"id": id})
	return
}
