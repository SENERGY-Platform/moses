package main

import "log"

func main() {
	log.Println("load config")
	config, err := LoadConfig()
	if err != nil {
		log.Fatal("unable to load config: ", err)
	}

	log.Println("connect to database")
	persistence, err := NewMongoPersistence(config)
	if err != nil {
		log.Fatal("unable to connect to database: ", err)
	}

	log.Println("load states")
	staterepo := &StateRepo{Persistence: persistence, Config: config}
	err = staterepo.Load()
	if err != nil {
		log.Fatal("unable to load state repo: ", err)
	}

	log.Println("start state routines")
	err = staterepo.Start()
	if err != nil {
		log.Fatal("unable to start state repo: ", err)
	}

	log.Println("start api on port: ", config.ServerPort)
	StartApi(config, staterepo)

}
