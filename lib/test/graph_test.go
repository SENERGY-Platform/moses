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

package test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/graphs"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	permClient "github.com/SENERGY-Platform/permissions-v2/pkg/client"
)

// TestEnvironmentGraphRoundtrip runs the mapping against the real
// device-repository, because the two facts the graph lifecycle is built on are
// not in the client's type signatures and would break silently:
//
//  1. the repository validates every graph it is given, and refuses one whose
//     edge weights are outside 1..100 or do not sum to 100 per node. A location
//     topology therefore carries a weight of 100 per edge, not 0.
//  2. the id of a new graph can only come from the repository. A put to an id
//     it does not know is a permission check against a resource that has no
//     permissions yet, and answers 403 - which is why the ref is taken from the
//     answer of the create instead of being generated in moses.
func TestEnvironmentGraphRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with docker containers in short mode")
	}
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	repoUrl, permissionsUrl, err := startDeviceRepo(ctx, wg)
	if err != nil {
		t.Fatal(err)
	}
	client := deviceRepo.NewClient(repoUrl, nil)

	const user = "8a1e5b0a-0000-4000-8000-000000000001"
	env := domain.Environment{
		Id:    "env-1",
		Name:  "Metallbau Musterstadt",
		Owner: user,
		Type:  domain.IndustrialSite,
		Zones: []domain.Zone{{
			Id: "zone-halle", Name: "Halle 1", Type: domain.ZoneHall,
			Assets: []domain.Asset{{
				Id: "asset-summe", Name: "Summe Halle", Kind: domain.AssetMeter,
			}},
			Zones: []domain.Zone{{
				Id: "zone-nebenraum", Name: "Nebenraum", Type: domain.ZoneRoom,
			}},
		}},
	}
	token := userToken(user)

	created, err, code := client.SetGraph(token, graphs.Build(env))
	if err != nil {
		t.Fatalf("the repository refused the mirror of an environment (%d): %v", code, err)
	}
	if created.Id == "" {
		t.Fatal("expected the repository to assign an id to a new graph")
	}
	env.ExternalGraphRef = created.Id

	read, err, code := client.ReadGraph(token, created.Id)
	if err != nil {
		t.Fatalf("unable to read the graph back (%d): %v", code, err)
	}
	if len(read.Nodes) != 3 || len(read.Edges) != 2 {
		t.Errorf("expected the root and two zones with an edge each, got %+v / %+v", read.Nodes, read.Edges)
	}
	if read.Owner != user {
		t.Errorf("expected the environment owner on the graph, got %q", read.Owner)
	}

	//a second save updates that graph instead of creating another
	env.Name = "Metallbau Musterstadt, neu"
	updated, err, code := client.SetGraph(token, graphs.Build(env))
	if err != nil {
		t.Fatalf("the repository refused the update of a mirror (%d): %v", code, err)
	}
	if updated.Id != created.Id {
		t.Errorf("expected the update to stay on the same graph, got %q after %q", updated.Id, created.Id)
	}
	all, _, err, code := client.ListGraphs(token, deviceRepo.GraphListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("unable to list graphs (%d): %v", code, err)
	}
	if len(all) != 1 {
		t.Errorf("expected the mirror to be one graph after two saves, got %d", len(all))
	}

	//the constraint that decides where the ref comes from
	t.Run("a self chosen id is refused for a graph that does not exist", func(t *testing.T) {
		invented := env
		invented.ExternalGraphRef = "urn:infai:ses:graph:invented-by-moses"
		_, err, code := client.SetGraph(token, graphs.Build(invented))
		if err == nil {
			t.Fatal("a graph id invented by the caller was accepted, the lifecycle could mint its own refs after all")
		}
		if code != http.StatusForbidden {
			t.Logf("refused with %d: %v", code, err)
		}
	})

	//a share with graph_writers adds write to an entry on the graph; the next
	//save of the environment must leave that entry and replace the writer's edit
	t.Run("a graph writer keeps write across a rewrite and loses the edit", func(t *testing.T) {
		const writer = "8a1e5b0a-0000-4000-8000-000000000002"
		permissions := permClient.New(permissionsUrl)
		resource, err, code := permissions.GetResource(string(helper.AdminJwt), "graphs", created.Id)
		if err != nil {
			t.Fatalf("unable to read the rights of the graph (%d): %v", code, err)
		}
		rights := resource.ResourcePermissions
		rights.UserPermissions[writer] = permClient.PermissionsMap{Read: true, Write: true, Execute: true}
		if _, err, code = permissions.SetPermission(string(helper.AdminJwt), "graphs", created.Id, rights); err != nil {
			t.Fatalf("unable to give the writer write (%d): %v", code, err)
		}

		edited := env
		edited.Name = "edited by the graph writer"
		if _, err, code := client.SetGraph(userToken(writer), graphs.Build(edited)); err != nil {
			t.Fatalf("write on the graph has to be enough to edit it (%d): %v", code, err)
		}
		if got := rootName(t, client, token, created.Id); got != edited.Name {
			t.Fatalf("expected the writer's edit to be stored, got %q", got)
		}
		if _, err, code := client.SetGraph(token, graphs.Build(env)); err != nil {
			t.Fatalf("the owner's rewrite was refused (%d): %v", code, err)
		}

		after, err, code := permissions.GetResource(string(helper.AdminJwt), "graphs", created.Id)
		if err != nil {
			t.Fatalf("unable to read the rights after the rewrite (%d): %v", code, err)
		}
		if got := after.UserPermissions[writer]; !got.Read || !got.Write || !got.Execute {
			t.Errorf("the rewrite must leave the writer's entry as it was, got %+v", got)
		}
		if got := after.UserPermissions[user]; !got.Administrate {
			t.Errorf("the owner has to keep administrate, got %+v", got)
		}
		if got := rootName(t, client, token, created.Id); got != env.Name {
			t.Errorf("the rewrite has to replace the writer's edit, got %q", got)
		}
	})

	//and the delete, which is what an environment's delete does
	if err, code := client.DeleteGraph(token, created.Id); err != nil {
		t.Fatalf("unable to delete the graph (%d): %v", code, err)
	}
	if _, err, _ := client.ReadGraph(token, created.Id); err == nil {
		t.Error("the graph survived its delete")
	}
	//deleting it again is not an error: the repository answers a delete of an
	//unknown graph with a success, which is what the best effort cleanup relies on
	if err, code := client.DeleteGraph(token, created.Id); err != nil && code != http.StatusNotFound {
		t.Errorf("a repeated delete has to be tolerated, got %d: %v", code, err)
	}
}

// startDeviceRepo brings up only what the graph api needs. The full server.New
// starts moses as well, which this test has no use for.
func startDeviceRepo(ctx context.Context, wg *sync.WaitGroup) (url string, permissionsUrl string, err error) {
	_, containerKafkaUrl, err := server.Kafka(ctx, wg)
	if err != nil {
		return "", "", err
	}
	_, mongoIp, err := server.MongoDB(ctx, wg)
	if err != nil {
		return "", "", err
	}
	containerMongoUrl := "mongodb://" + mongoIp + ":27017"
	permV2HostPort, permV2Ip, err := server.PermissionsV2(ctx, wg, containerMongoUrl, containerKafkaUrl)
	if err != nil {
		return "", "", err
	}
	repoHostPort, _, err := server.DeviceRepo(ctx, wg, containerKafkaUrl, containerMongoUrl, "http://"+permV2Ip+":8080")
	if err != nil {
		return "", "", err
	}
	return "http://localhost:" + repoHostPort, "http://localhost:" + permV2HostPort, nil
}

// rootName is the display name of a graph, the name attribute of its root node.
func rootName(t *testing.T, client deviceRepo.Interface, token string, id string) string {
	t.Helper()
	read, err, code := client.ReadGraph(token, id)
	if err != nil {
		t.Fatalf("unable to read the graph (%d): %v", code, err)
	}
	for _, node := range read.Nodes {
		if node.Id != graphs.RootNodeId {
			continue
		}
		for _, attribute := range node.Attributes {
			if attribute.Key == graphs.NameAttribute {
				return attribute.Value
			}
		}
	}
	return ""
}

// userToken is a token for a plain user. The shared AdminJwt carries the admin
// realm role, which skips exactly the permission checks this test is about.
// Signatures are checked at the gateway, not by the services.
func userToken(userId string) string {
	payload, err := json.Marshal(map[string]interface{}{
		"sub":          userId,
		"realm_access": map[string]interface{}{"roles": []string{"user"}},
	})
	if err != nil {
		panic(err)
	}
	encode := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return "Bearer " + encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".signature"
}
