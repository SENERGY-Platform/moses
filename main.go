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

package main

import (
	"context"
	"github.com/SENERGY-Platform/moses/lib"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.Println("load config")
	config, err := lib.LoadConfig()
	if err != nil {
		log.Fatal("unable to load config: ", err)
	}

	time.Sleep(5 * time.Second) //wait for routing tables in cluster

	ctx, cancel := context.WithCancel(context.Background())

	err = lib.New(config, ctx)
	if err != nil {
		log.Println(err)
		cancel()
	}

	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
		sig := <-shutdown
		log.Println("received shutdown signal", sig)
		cancel()
	}()

	<-ctx.Done()                //waiting for context end; may happen by shutdown signal
	time.Sleep(1 * time.Second) //give go routines time for cleanup
}
