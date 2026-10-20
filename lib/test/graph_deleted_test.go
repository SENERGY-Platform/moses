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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	deviceRepo "github.com/SENERGY-Platform/device-repository/lib/client"
	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/domain"
	"github.com/SENERGY-Platform/moses/lib/test/helper"
	"github.com/SENERGY-Platform/moses/lib/test/server"
	permClient "github.com/SENERGY-Platform/permissions-v2/pkg/client"
)

// TestAGraphAdminDeletesTheGraphAndTheNextSaveCreatesItAgain runs moses against
// the real device-repository and permissions-v2, because the replacement rests
// on two answers no type states: the repository refuses a write to a deleted
// graph with 403, and permissions-v2 then answers 404 "not found" for its rights.
func TestAGraphAdminDeletesTheGraphAndTheNextSaveCreatesItAgain(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test with docker containers in short mode")
	}
	wg := &sync.WaitGroup{}
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultConfig, err := config.LoadConfigLocation("../../config.json")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := server.New(ctx, wg, defaultConfig, "./server/keycloak-export.json")
	if err != nil {
		t.Fatal(err)
	}

	const owner = "8a1e5b0a-0000-4000-8000-000000000011"
	const admin = "8a1e5b0a-0000-4000-8000-000000000012"
	ownerToken, adminToken := userToken(owner), userToken(admin)
	base := "http://localhost:" + conf.ServerPort
	waitForApi(t, base, ownerToken)

	created := domain.Environment{}
	if code := callMoses(t, http.MethodPost, base+"/environments", ownerToken, domain.Environment{
		Name: "Metallbau Musterstadt", Type: domain.IndustrialSite,
		Zones: []domain.Zone{{Id: "zone-halle", Name: "Halle 1", Type: domain.ZoneHall}},
	}, &created); code != http.StatusCreated {
		t.Fatalf("expected the create to succeed, got %d", code)
	}
	first := created.ExternalGraphRef
	if first == "" {
		t.Fatal("expected the create to mirror the environment as a graph")
	}

	if code := callMoses(t, http.MethodPut, base+"/environments/"+created.Id+"/shares", ownerToken, map[string]interface{}{
		"users":        []string{admin},
		"graph_admins": map[string]interface{}{"users": []string{admin}},
	}, nil); code != http.StatusOK {
		t.Fatalf("expected the share to succeed, got %d", code)
	}

	repo := deviceRepo.NewClient(conf.DeviceRepoUrl, nil)
	if err, code := repo.DeleteGraph(adminToken, first); err != nil {
		t.Fatalf("a graph admin has to be able to delete the graph (%d): %v", code, err)
	}
	if _, err, _ := repo.ReadGraph(ownerToken, first); err == nil {
		t.Fatal("the graph survived its delete")
	}

	saved := domain.Environment{}
	if code := callMoses(t, http.MethodPut, base+"/environments/"+created.Id, ownerToken, created, &saved); code != http.StatusOK {
		t.Fatalf("expected the save to succeed, got %d", code)
	}
	second := saved.ExternalGraphRef
	if second == "" || second == first {
		t.Fatalf("expected the save to create a new graph, got %q after %q", second, first)
	}
	if got := rootName(t, repo, ownerToken, second); got != created.Name {
		t.Errorf("expected the new graph to mirror the environment, got %q", got)
	}
	rights, err, code := permClient.New(conf.PermissionsV2Url).GetResource(string(helper.AdminJwt), "graphs", second)
	if err != nil {
		t.Fatalf("unable to read the rights of the new graph (%d): %v", code, err)
	}
	if got := rights.UserPermissions[admin]; !got.Administrate || !got.Write || !got.Read {
		t.Errorf("the graph admin has to administrate the new graph, got %+v", got)
	}
	if got := rights.UserPermissions[owner]; !got.Administrate {
		t.Errorf("the owner has to administrate the new graph, got %+v", got)
	}

	again := domain.Environment{}
	if code := callMoses(t, http.MethodPut, base+"/environments/"+created.Id, ownerToken, saved, &again); code != http.StatusOK {
		t.Fatalf("expected the second save to succeed, got %d", code)
	}
	if again.ExternalGraphRef != second {
		t.Errorf("a graph that exists is rewritten, not replaced, got %q after %q", again.ExternalGraphRef, second)
	}
	all, _, err, code := repo.ListGraphs(ownerToken, deviceRepo.GraphListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("unable to list graphs (%d): %v", code, err)
	}
	if len(all) != 1 {
		t.Errorf("expected exactly one graph for the environment, got %d", len(all))
	}
}

// waitForApi polls until moses answers, which it does only after its startup.
func waitForApi(t *testing.T, base string, token string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequest(http.MethodGet, base+"/environments", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", token)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("moses did not start answering")
}

// callMoses sends body as JSON and decodes the answer into result when it is a
// success.
func callMoses(t *testing.T, method string, url string, token string, body interface{}, result interface{}) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	answer, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 300 {
		t.Logf("%s %s answered %d: %s", method, url, response.StatusCode, string(answer))
		return response.StatusCode
	}
	if result != nil {
		if err = json.Unmarshal(answer, result); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode
}
