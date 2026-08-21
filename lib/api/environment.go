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

// EnvironmentEndpoints serves the environment model. An environment is one
// document: GET returns exactly what PUT accepts, so an export can be edited
// and put back, and a whole site can be created in a single call.
//
// Each route lives in its own function returning method, path and handler. That
// is not decoration: swaggo reads annotations above a function declaration only,
// so a route registered inside a closure cannot be documented.
func EnvironmentEndpoints(config config.Config, environments repo.Environments, notifier RuntimeNotifier, router gin.IRouter) {
	for _, route := range []func(repo.Environments, RuntimeNotifier) (string, string, gin.HandlerFunc){
		listEnvironmentsH,
		getEnvironmentH,
		putEnvironmentH,
		postEnvironmentH,
		deleteEnvironmentH,
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
			// deliberately 404 and not 403: whether an environment exists is
			// not information a caller without access should get
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
		// the path wins over the body, so that a document can be copied to a
		// new id without editing it first
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
		// only this environment is reloaded, and only after it was stored: the
		// runtime reads the definition back, so notifying before the write would
		// restart it on the old one
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

// requireUser reads the caller's token. Every handler in this service, the
// legacy ones included, goes through here, which makes this the trust boundary.
//
// TRUST BOUNDARY, BY DESIGN: the token is parsed, not verified. service-commons
// also offers GetParsedAndValidatedToken and this deliberately does not use it.
// In the SENERGY platform the api gateway checks the signature and the standard
// claims before a request reaches a service, and the sibling services behind it
// parse only. Checking again here would need a keycloak cert provider in every
// service and would take moses down whenever keycloak is briefly unreachable,
// without adding a guarantee the gateway does not already give.
//
// So a garbage signature, an expired token and an unknown issuer all pass. The
// tests in environment_test.go pin that on purpose, so nobody reads the missing
// verification as an oversight and nobody drops it as dead weight. If moses is
// ever exposed without a validating gateway in front of it, this function is the
// one place that has to change.
//
// The check that is ours: a payload carrying no subject parses without error and
// yields an empty user id, which would otherwise be stored as the owner and then
// match every other subjectless token. That is an authentication failure, 401. A
// token that cannot be parsed at all is a malformed request, 400.
//
// The returned token carries the raw credential in a field, because downstream
// calls forward it. It must never be serialised into a response or a log line;
// pass GetUserId() around instead.
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

// mayAccess is the single place that decides access. It is owner based for now;
// sharing through permissions-v2 replaces the body of this function without
// touching any handler.
func mayAccess(token sc_jwt.Token, env domain.Environment) bool {
	return env.Owner == token.GetUserId() || token.IsAdmin()
}

// writeValidationError returns every problem with its path, so that a caller
// can mark the offending fields instead of reporting that something is wrong.
func writeValidationError(gc *gin.Context, err error) {
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) {
		gc.String(http.StatusBadRequest, "%s", err.Error())
		return
	}
	gc.JSON(http.StatusBadRequest, invalid)
}
