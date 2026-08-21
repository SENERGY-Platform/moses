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

package api

import (
	"errors"
	"net/http"

	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
)

// GetStatusCode maps an error a handler passed to gin (gc.Error) to a status
// code. It is the single place that decides what an error means to a caller.
//
// Note what it deliberately does NOT do: it never puts the error text in the
// response. A driver error carries hostnames and replica set topology, and that
// is not information a caller should receive.
func GetStatusCode(err error) int {
	var invalid *domain.ValidationError
	switch {
	case errors.As(err, &invalid):
		return http.StatusBadRequest
	case errors.Is(err, repo.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
