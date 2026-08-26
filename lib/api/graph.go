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

	"github.com/SENERGY-Platform/go-service-base/struct-logger/attributes"
	"github.com/SENERGY-Platform/models/go/models"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/graphs"
	"github.com/SENERGY-Platform/moses/lib/util"
	sc_jwt "github.com/SENERGY-Platform/service-commons/pkg/jwt"
)

// reconcileGraphRef decides which graph the document about to be stored owns.
// Same rule and same reason as reconcileManagedFlags: the client sends the whole
// document back, so the ref it carries is worth nothing.
//
//   - An update of a stored document keeps the stored ref. A client that sends a
//     different one is trying to make this environment write into a graph it does
//     not own.
//   - Anything else starts without a ref and gets a fresh graph. The case this
//     is for is the export copied to a new id: its ref still points at the graph
//     of the original, and honouring it would have the copy overwrite the
//     original's graph on save and delete it on delete.
//
// previous is nil exactly when nothing is stored under this id - a create, or a
// put to an id that is new here.
func reconcileGraphRef(previous *domain.Environment, env *domain.Environment) {
	if previous == nil {
		env.ExternalGraphRef = ""
		return
	}
	env.ExternalGraphRef = previous.ExternalGraphRef
}

// GraphMirror is what the api needs from the device-repository's graph api.
// Narrow because it is a boundary: two methods can be faked in a test, the full
// client cannot.
type GraphMirror interface {
	// SetGraph creates a graph when graph.Id is empty and replaces it otherwise.
	// The returned graph carries the id the repository assigned.
	SetGraph(token string, graph models.Graph) (models.Graph, error, int)
	DeleteGraph(token string, id string) (error, int)
}

// mirrorGraph writes the environment to the device-repository as a graph and
// records the id it got under. Called with the document as it will be stored,
// after provisioning, so the device references the graph needs are set.
//
// It runs BEFORE the write, unlike the device cleanup, and the difference is
// deliberate. The id of a graph that does not exist yet can only come from the
// repository - a self-invented id is refused, because a put to an unknown graph
// id is a permission check against a resource that has no permissions yet. So
// either the graph is written first and the document stores the ref it returned,
// or the document is written twice. Mirroring first costs a graph that outlives
// a failed write; writing twice costs a second write that can fail on its own
// and then leaks a graph nothing references. The first is the cheaper failure,
// and the same one the delete path already accepts.
//
// Best effort throughout: a repository that is down leaves a stale or missing
// graph and a warning, and the save succeeds. The mirror exists for other
// applications to read; refusing to store a simulation because a reader is
// unavailable would be the wrong trade.
//
// The graph is rebuilt in full every time, so it is a mirror and not a document:
// a node moved or renamed by hand in a graph editor does not survive the next
// save of the environment.
func mirrorGraph(mirror GraphMirror, token sc_jwt.Token, env *domain.Environment) {
	if mirror == nil {
		return
	}
	graph := graphs.Build(*env)
	result, err, code := mirror.SetGraph(token.Jwt(), graph)
	if err != nil {
		util.Logger.Warn("unable to mirror an environment as a graph", attributes.ErrorKey, err,
			"environment", env.Id, "graph", graph.Id, "status", code)
		return
	}
	// the repository assigns the id of a new graph; on an update it echoes the
	// one that was sent. An empty answer is not allowed to blank a ref that
	// worked - that would orphan the graph and create a second one next time.
	if result.Id != "" {
		env.ExternalGraphRef = result.Id
	}
}

// deleteGraph removes the mirror of a deleted environment. After the delete of
// the document, for the same reason deleteDevices runs there: a failed delete
// leaves the environment, and it keeps its graph.
//
// Best effort as well. What a failure leaves behind is a graph without an
// environment, which is recoverable by hand - the opposite, a delete that fails
// over an unreachable reader, is not something a caller can do anything about.
func deleteGraph(mirror GraphMirror, token sc_jwt.Token, env *domain.Environment) {
	if mirror == nil || env.ExternalGraphRef == "" {
		return
	}
	//a graph that is already gone is what the caller wanted. The repository
	//answers a delete of an unknown graph with a success of its own, so this
	//covers the graph somebody removed by hand and a retry after a partial
	//cleanup
	if err, code := mirror.DeleteGraph(token.Jwt(), env.ExternalGraphRef); err != nil && code != http.StatusNotFound {
		util.Logger.Warn("unable to delete the graph of a removed environment", attributes.ErrorKey, err,
			"environment", env.Id, "graph", env.ExternalGraphRef, "status", code)
		return
	}
	util.Logger.Info("deleted the graph of a removed environment", "environment", env.Id, "graph", env.ExternalGraphRef)
}
