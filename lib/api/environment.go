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
	"os"

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
	"github.com/gin-gonic/gin"
)

func init() {
	environmentEndpoints = append(environmentEndpoints, EnvironmentEndpoints)
}

// EnvironmentEndpoints serves the environment model. GET returns exactly what
// PUT accepts, so an export can be edited and put back.
//
// Each route is its own function returning method, path and handler, because
// swaggo reads annotations above a function declaration only.
func EnvironmentEndpoints(config config.Config, environments repo.Environments, notifier RuntimeNotifier, router gin.IRouter) {
	for _, route := range []func(repo.Environments, RuntimeNotifier) (string, string, gin.HandlerFunc){
		listEnvironmentsH,
		getEnvironmentH,
		putEnvironmentH,
		postEnvironmentH,
		deleteEnvironmentH,
		patchEnvironmentStateH,
		getSwaggerDocH,
	} {
		method, path, handler := route(environments, notifier)
		router.Handle(method, path, handler)
	}
}

// @Summary List environments
// @Description Every environment owned by the caller, ordered by name. Empty list, never null.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Success 200 {array} domain.Environment
// @Failure 400 {string} string "the token is missing or unreadable"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /environments [get]
func listEnvironmentsH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		result, err := environments.ListByOwner(gc.Request.Context(), token.GetUserId())
		if err != nil {
			util.Logger.Error("unable to list environments", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to list environments")
			return
		}
		gc.JSON(http.StatusOK, result)
	}
}

// @Summary Export one environment
// @Description Returns exactly what PUT accepts, so an export can be edited and put back.
// @Tags Environment
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Success 200 {object} domain.Environment
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, or no access to it"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [get]
func getEnvironmentH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, err := environments.Get(gc.Request.Context(), gc.Param("id"))
		if errors.Is(err, repo.ErrNotFound) {
			gc.String(http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, env) {
			// 404 not 403: existence is not information for a caller
			// without access
			gc.String(http.StatusNotFound, "not found")
			return
		}
		gc.JSON(http.StatusOK, env)
	}
}

// @Summary Create or replace one environment
// @Description Idempotent. The id in the path wins over the one in the body, so a document can be copied to a new id without editing it. Ownership comes from the token on create and never transfers on update.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param environment body domain.Environment true "the environment"
// @Success 200 {object} domain.Environment
// @Failure 400 {object} domain.ValidationError "every problem, with the path of the offending field"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "the environment belongs to somebody else"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [put]
func putEnvironmentH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPut, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env := domain.Environment{}
		err := gc.ShouldBindJSON(&env)
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as an environment: %s", err.Error())
			return
		}
		// path wins over body, so a document can be copied to a new id
		env.Id = gc.Param("id")

		existing, err := environments.Get(gc.Request.Context(), env.Id)
		switch {
		case err == nil:
			if !mayAccess(token, existing) {
				gc.String(http.StatusNotFound, "not found")
				return
			}
			// ownership never transfers through an import
			env.Owner = existing.Owner
		case errors.Is(err, repo.ErrNotFound):
			env.Owner = token.GetUserId()
		default:
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}

		domain.AssignIds(&env)

		err = domain.Validate(env)
		if err != nil {
			writeValidationError(gc, err)
			return
		}

		err = environments.Put(gc.Request.Context(), env)
		if err != nil {
			util.Logger.Error("unable to store environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to store environment")
			return
		}
		// after the write: the runtime reads the definition back, so notifying
		// earlier would restart it on the old one
		notifyReload(notifier, env.Id)
		gc.JSON(http.StatusOK, env)
	}
}

// @Summary Create an environment with a server assigned id
// @Description Any id in the body is ignored. Nested entities may omit their ids and get one assigned.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param environment body domain.Environment true "the environment"
// @Success 201 {object} domain.Environment
// @Failure 400 {object} domain.ValidationError "every problem, with the path of the offending field"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 500 {string} string "error message"
// @Router /environments [post]
func postEnvironmentH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPost, "/environments", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env := domain.Environment{}
		err := gc.ShouldBindJSON(&env)
		if err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as an environment: %s", err.Error())
			return
		}
		env.Id = ""
		env.Owner = token.GetUserId()
		domain.AssignIds(&env)

		err = domain.Validate(env)
		if err != nil {
			writeValidationError(gc, err)
			return
		}
		err = environments.Put(gc.Request.Context(), env)
		if err != nil {
			util.Logger.Error("unable to store environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to store environment")
			return
		}
		notifyReload(notifier, env.Id)
		gc.JSON(http.StatusCreated, env)
	}
}

// @Summary Delete one environment and its runtime state
// @Tags Environment
// @Security Bearer
// @Param id path string true "environment id"
// @Success 204 {string} string "deleted, or there was nothing to delete"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "the environment belongs to somebody else"
// @Failure 500 {string} string "error message"
// @Router /environments/{id} [delete]
func deleteEnvironmentH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodDelete, "/environments/:id", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		env, err := environments.Get(gc.Request.Context(), gc.Param("id"))
		if errors.Is(err, repo.ErrNotFound) {
			// deleting something that is not there is not an error
			gc.Status(http.StatusNoContent)
			return
		}
		if err != nil {
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, env) {
			gc.String(http.StatusNotFound, "not found")
			return
		}
		err = environments.Delete(gc.Request.Context(), env.Id)
		if err != nil {
			util.Logger.Error("unable to delete environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to delete environment")
			return
		}
		notifyRemove(notifier, env.Id)
		gc.Status(http.StatusNoContent)
	}
}

// patchEnvironmentStateH turns a boundary condition from outside the simulation.
// The room climate case is what it exists for: set a hall temperature and the
// sensors in that hall pick it up on their next tick, which is what the legacy
// world/room state was reaching for and never finished.
//
// @Summary Set live state of one running environment
// @Description Merges the given values into the live state. Only the keys named are touched; everything else keeps running. Context is the shared surroundings every zone reads, zones and assets are keyed by their id from the definition. Takes effect on the next tick of the scripts that read it, and is not a change to the definition.
// @Tags Environment
// @Accept json
// @Produce json
// @Security Bearer
// @Param id path string true "environment id"
// @Param state body repo.StateChange true "the values to set"
// @Success 204 {string} string "applied"
// @Failure 400 {string} string "the body is unreadable, empty, or names a zone or asset the definition does not have"
// @Failure 401 {string} string "the token carries no subject"
// @Failure 404 {string} string "no such environment, no access to it, or it is not running here"
// @Failure 500 {string} string "error message"
// @Router /environments/{id}/state [patch]
func patchEnvironmentStateH(environments repo.Environments, notifier RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodPatch, "/environments/:id/state", func(gc *gin.Context) {
		token, ok := requireUser(gc)
		if !ok {
			return
		}
		id := gc.Param("id")

		existing, err := environments.Get(gc.Request.Context(), id)
		switch {
		case errors.Is(err, repo.ErrNotFound):
			gc.String(http.StatusNotFound, "not found")
			return
		case err != nil:
			util.Logger.Error("unable to read environment", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "unable to read environment")
			return
		}
		if !mayAccess(token, existing) {
			gc.String(http.StatusNotFound, "not found")
			return
		}

		change := repo.StateChange{}
		if err = gc.ShouldBindJSON(&change); err != nil {
			gc.String(http.StatusBadRequest, "unable to read the request body as a state change: %s", err.Error())
			return
		}
		if change.Empty() {
			gc.String(http.StatusBadRequest, "the state change is empty, so it would do nothing")
			return
		}

		err = setState(notifier, id, change)
		unknownIds := &repo.UnknownIdsError{}
		switch {
		case err == nil:
			gc.Status(http.StatusNoContent)
		case errors.As(err, &unknownIds):
			gc.String(http.StatusBadRequest, "%s", unknownIds.Error())
		case errors.Is(err, repo.ErrNotRunning), errors.Is(err, ErrNoRuntime):
			//404 and not 409: from outside, an environment this instance does not
			//run is indistinguishable from one that does not exist here
			gc.String(http.StatusNotFound, "the environment is not running here")
		default:
			util.Logger.Error("unable to set the environment state", attributes.ErrorKey, err, "environment", id)
			gc.String(http.StatusInternalServerError, "unable to set the environment state")
		}
	}
}

// @Summary The generated openapi specification of this service
// @Tags Doc
// @Produce json
// @Success 200 {string} string "the specification"
// @Failure 500 {string} string "error message"
// @Router /doc [get]
func getSwaggerDocH(_ repo.Environments, _ RuntimeNotifier) (string, string, gin.HandlerFunc) {
	return http.MethodGet, "/doc", func(gc *gin.Context) {
		// generated at image build time by go generate, not committed
		if _, err := os.Stat(swaggerDocPath); err != nil {
			util.Logger.Warn("the openapi specification is not present", attributes.ErrorKey, err)
			gc.String(http.StatusInternalServerError, "the specification was not generated for this build")
			return
		}
		gc.Header("Content-Type", gin.MIMEJSON)
		gc.File(swaggerDocPath)
	}
}

const swaggerDocPath = "docs/swagger.json"

// requireUser reads the caller's token. Every handler goes through here, which
// makes it the trust boundary.
//
// TRUST BOUNDARY, BY DESIGN: the token is parsed, not verified - the api gateway
// checks signature and claims first, as it does for the sibling services, so a
// garbage signature or expired token passes here. environment_test.go pins that
// so it is not read as an oversight. Expose moses without a validating gateway
// and this function is the one place to change.
//
// The check that is ours: a payload without a subject parses fine and yields an
// empty user id, which would be stored as owner and match every other
// subjectless token. That is a 401; an unparseable token is a 400.
//
// The returned token carries the raw credential, because downstream calls
// forward it. Never serialise it into a response or a log line.
func requireUser(gc *gin.Context) (sc_jwt.Token, bool) {
	token, err := sc_jwt.GetParsedToken(gc.Request)
	if err != nil {
		gc.String(http.StatusBadRequest, "%s", err.Error())
		return token, false
	}
	if token.GetUserId() == "" {
		gc.String(http.StatusUnauthorized, "the token carries no subject")
		return token, false
	}
	return token, true
}

// mayAccess is the single place that decides access; owner based for now,
// permissions-v2 sharing replaces the body without touching a handler.
func mayAccess(token sc_jwt.Token, env domain.Environment) bool {
	return env.Owner == token.GetUserId() || token.IsAdmin()
}

// writeValidationError returns every problem with its path, so a caller can
// mark the offending fields.
func writeValidationError(gc *gin.Context, err error) {
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) {
		gc.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	gc.JSON(http.StatusBadRequest, invalid)
}
