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

//go:generate go run github.com/swaggo/swag/cmd/swag@v1.16.4 init --generalInfo lib/api/api.go --output docs --parseDependency --parseInternal --outputTypes json,yaml

import (
	"context"
	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/util"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	config, err := config.LoadConfig()
	if err != nil {
		// the logger is not configured yet, so this one stays on the standard library
		log.Fatal("unable to load config: ", err)
	}
	util.InitLogger(config.LoggerHandler, config.LoggerLevel)

	time.Sleep(5 * time.Second) //wait for routing tables in cluster

	ctx, cancel := context.WithCancel(context.Background())

	err = lib.New(config, ctx)
	if err != nil {
		util.Logger.Error("unable to start", attributes.ErrorKey, err)
		cancel()
	}

	go func() {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
		sig := <-shutdown
		util.Logger.Info("received shutdown signal", "signal", sig.String())
		cancel()
	}()

	<-ctx.Done()                //waiting for context end; may happen by shutdown signal
	time.Sleep(1 * time.Second) //give go routines time for cleanup
}
