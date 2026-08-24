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

// Package util holds the process wide structured logger, wired the way the other
// platform services wire struct-logger.
package util

import (
	"log/slog"
	"os"

	struct_logger "github.com/SENERGY-Platform/go-service-base/struct-logger"
)

const (
	organization = "github.com/SENERGY-Platform"
	project      = "moses"
)

// Logger is replaced once by InitLogger at start up; until then it writes to
// stderr, so logging from an init function or a test cannot panic.
var Logger *slog.Logger = struct_logger.New(struct_logger.Config{Handler: "text", Level: "info"}, os.Stderr, organization, project)

// InitLogger configures the logger from the service configuration.
//
// ERROR raises an automatic notification, so only failures that need someone to
// act belong there; an expected or self healing condition is a WARN.
func InitLogger(handler string, level string) {
	if handler == "" {
		handler = "json"
	}
	if level == "" {
		level = "info"
	}
	Logger = struct_logger.New(struct_logger.Config{
		Handler:   handler,
		Level:     level,
		AddSource: true,
	}, os.Stderr, organization, project)
	slog.SetDefault(Logger)
}
