package main

import (
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
)

type PersistenceInterface interface {
	PersistWorld(world World) (err error)
	PersistGraph(graph Graph) (err error)
	LoadWorlds() (map[string]World, error)
	LoadGraphs() (map[string]Graph, error)
}

type MongoPersistence struct {
	session             *mgo.Session
	worldCollectionName string
	graphCollectionName string
	tableName           string
}

func NewMongoPersistence(config Config) (result MongoPersistence, err error) {
	result.worldCollectionName = config.WorldCollectionName
	result.graphCollectionName = config.GraphCollectionName
	result.session, err = mgo.Dial(config.MongoUrl)
	if err == nil {
		result.session.SetMode(mgo.Monotonic, true)
	}
	return
}

func (this MongoPersistence) getWorldCollection() (session *mgo.Session, collection *mgo.Collection) {
	collection = session.DB(this.tableName).C(this.worldCollectionName)
	return
}

func (this MongoPersistence) getGraphCollection() (session *mgo.Session, collection *mgo.Collection) {
	collection = session.DB(this.tableName).C(this.graphCollectionName)
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

func (this MongoPersistence) LoadWorlds() (result map[string]World, err error) {
	session, collection := this.getWorldCollection()
	defer session.Close()
	worlds := []World{}
	err = collection.Find(nil).All(&worlds)
	if err != nil {
		return result, err
	}
	result = map[string]World{}
	for _, world := range worlds {
		result[world.Id] = world
	}
	return
}

func (this MongoPersistence) LoadGraphs() (result map[string]Graph, err error) {
	session, collection := this.getGraphCollection()
	defer session.Close()
	graphs := []Graph{}
	err = collection.Find(nil).All(&graphs)
	if err != nil {
		return result, err
	}
	result = map[string]Graph{}
	for _, graph := range graphs {
		result[graph.Id] = graph
	}
	return
}
