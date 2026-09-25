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
	"net/http"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/effects"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/gin-gonic/gin"
)

func init() {
	environmentEndpoints = append(environmentEndpoints, EnvironmentEffectsEndpoints)
}

// EnvironmentEffectsEndpoints serve the effect graph derived from a stored
// definition. It reads the document only, never the runtime.
func EnvironmentEffectsEndpoints(config config.Config, environments repo.Environments, shares repo.Shares, catalog DeviceCatalog, mirror GraphMirror, notifier RuntimeNotifier, permissions Permissions, router gin.IRouter) {
	method, path, handler := getEnvironmentEffectsH(environments)
	router.Handle(method, path, handler)
}

// @Summary Effect graph of one environment
// @Description Which context keys, assets and zones drive which others, derived from the stored definition alone: script reads and writes of foreign state, formula inputs, schedule gates and scales, the meter tree and its aggregates, and the dated changes of the timeline. Edges point from the producer to the consumer.
// @Description
// @Description A script is analysed, not run. A reference whose zone, asset or key is not a string literal, an alias of one or a row of a literal table is listed under `unresolved` with the reason instead of being guessed; the rest of the graph is derived all the same.
// @Description
// @Description Access is that of GET /environments/{id}: a missing environment and one the caller may not see are both 404. See docs/effects.md.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} effects.Graph
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/effects [get]
func getEnvironmentEffectsH(environments repo.Environments) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id/effects", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, ok := accessibleEnvironment(gc, environments, token, gc.Param("id"))
		if !ok {
			return
		}
		gc.JSON(http.StatusOK, effects.Derive(env))
	}
}
