package server

import (
	"context"
	"github.com/ory/dockertest"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"log"
	"time"
)

func MongoTestServer(pool *dockertest.Pool) (closer func(), hostPort string, ipAddress string, err error) {
	log.Println("start mongodb")
	repo, err := pool.Run("mongo", "4.1.11", []string{})
	if err != nil {
		return func() {}, "", "", err
	}
	hostPort = repo.GetPort("27017/tcp")
	err = pool.Retry(func() error {
		log.Println("try mongodb connection...")
		ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
		client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:"+hostPort))
		err = client.Ping(ctx, readpref.Primary())
		return err
	})
	return func() { repo.Close() }, hostPort, repo.Container.NetworkSettings.IPAddress, err
}
