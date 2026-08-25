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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/SENERGY-Platform/moses/lib/config"
	"github.com/SENERGY-Platform/moses/lib/repo"
	"github.com/gin-gonic/gin"
)

type fakeDatasets struct {
	mux     sync.Mutex
	stored  map[string]repo.DatasetMeta
	content map[string][]byte
}

func newFakeDatasets() *fakeDatasets {
	return &fakeDatasets{stored: map[string]repo.DatasetMeta{}, content: map[string][]byte{}}
}

func (this *fakeDatasets) Create(ctx context.Context, meta repo.DatasetMeta, raw []byte) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	this.stored[meta.Id] = meta
	this.content[meta.Id] = raw
	return nil
}

func (this *fakeDatasets) Get(ctx context.Context, id string) (repo.DatasetMeta, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	meta, ok := this.stored[id]
	if !ok {
		return meta, repo.ErrNotFound
	}
	return meta, nil
}

func (this *fakeDatasets) ListByOwner(ctx context.Context, owner string) ([]repo.DatasetMeta, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []repo.DatasetMeta{}
	for _, meta := range this.stored {
		if meta.Owner == owner {
			result = append(result, meta)
		}
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Name < result[b].Name })
	return result, nil
}

func (this *fakeDatasets) All(ctx context.Context) ([]repo.DatasetMeta, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	result := []repo.DatasetMeta{}
	for _, meta := range this.stored {
		result = append(result, meta)
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Name < result[b].Name })
	return result, nil
}

func (this *fakeDatasets) Content(ctx context.Context, id string) ([]byte, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	raw, ok := this.content[id]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return raw, nil
}

func (this *fakeDatasets) Delete(ctx context.Context, id string) error {
	this.mux.Lock()
	defer this.mux.Unlock()
	delete(this.stored, id)
	delete(this.content, id)
	return nil
}

func datasetRouter(store repo.Datasets) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	DatasetEndpoints(config.Config{}, store, router)
	return router
}

func upload(t *testing.T, router *gin.Engine, path string, userId string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", tokenFor(userId))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

const germanCSV = "Zeit;Wirkleistung\n05.01.2026 00:00;1,5\n05.01.2026 00:15;2,5\n"

func TestUploadParsesStoresAndAnswersTheMetadata(t *testing.T) {
	store := newFakeDatasets()
	router := datasetRouter(store)

	resp := upload(t, router, "/datasets?name=Lastgang", "user-a", germanCSV)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	meta := repo.DatasetMeta{}
	if err := json.Unmarshal(resp.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Id == "" || meta.Name != "Lastgang" || meta.Timezone != "Europe/Berlin" {
		t.Errorf("unexpected metadata: %+v", meta)
	}
	if len(meta.Columns) != 1 || meta.Columns[0].Name != "Wirkleistung" || meta.Columns[0].Points != 2 {
		t.Errorf("the parse result has to be in the metadata: %+v", meta.Columns)
	}
	raw, err := store.Content(context.Background(), meta.Id)
	if err != nil || string(raw) != germanCSV {
		t.Errorf("the raw file has to be stored byte for byte: %v %q", err, raw)
	}
	if stored, _ := store.Get(context.Background(), meta.Id); stored.Owner != "user-a" {
		t.Errorf("the owner comes from the token, got %q", stored.Owner)
	}
}

func TestUploadRefusals(t *testing.T) {
	router := datasetRouter(newFakeDatasets())
	for _, tc := range []struct{ name, path, body, fragment string }{
		{"missing name", "/datasets", germanCSV, "name query parameter"},
		{"unknown timezone", "/datasets?name=x&tz=Mars/Olympus", germanCSV, "unknown timezone"},
		{"broken file", "/datasets?name=x", "kaputt", "unable to parse"},
		{"parse error names the line", "/datasets?name=x", "t,v\nkaputt,1\n2026-01-05 00:15,2\n", "line 2"},
	} {
		resp := upload(t, router, tc.path, "user-a", tc.body)
		if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), tc.fragment) {
			t.Errorf("%s: expected 400 with %q, got %d: %s", tc.name, tc.fragment, resp.Code, resp.Body.String())
		}
	}
}

func TestDatasetsAreInvisibleAcrossOwners(t *testing.T) {
	store := newFakeDatasets()
	router := datasetRouter(store)
	resp := upload(t, router, "/datasets?name=geheim", "user-a", germanCSV)
	meta := repo.DatasetMeta{}
	_ = json.Unmarshal(resp.Body.Bytes(), &meta)

	if resp := do(t, router, "GET", "/datasets/"+meta.Id, "user-b", nil); resp.Code != http.StatusNotFound {
		t.Errorf("a foreign dataset has to be a 404, got %d", resp.Code)
	}
	if resp := do(t, router, "DELETE", "/datasets/"+meta.Id, "user-b", nil); resp.Code != http.StatusNotFound {
		t.Errorf("a foreign delete has to be a 404, got %d", resp.Code)
	}
	if _, err := store.Get(context.Background(), meta.Id); err != nil {
		t.Error("the foreign delete must not have removed anything")
	}
	if resp := do(t, router, "GET", "/datasets", "user-b", nil); strings.Contains(resp.Body.String(), meta.Id) {
		t.Error("a foreign dataset must not appear in the list")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	store := newFakeDatasets()
	router := datasetRouter(store)
	resp := upload(t, router, "/datasets?name=weg", "user-a", germanCSV)
	meta := repo.DatasetMeta{}
	_ = json.Unmarshal(resp.Body.Bytes(), &meta)

	if resp := do(t, router, "DELETE", "/datasets/"+meta.Id, "user-a", nil); resp.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.Code)
	}
	if resp := do(t, router, "DELETE", "/datasets/"+meta.Id, "user-a", nil); resp.Code != http.StatusNoContent {
		t.Errorf("deleting nothing is not an error, got %d", resp.Code)
	}
}

// An administrator sees every dataset, for the same reason as the environments:
// requireDataset lets one open a foreign dataset, so hiding it in the list would
// only make it unfindable.
func TestAnAdminListsAndOpensEveryDataset(t *testing.T) {
	store := newFakeDatasets()
	router := datasetRouter(store)
	resp := upload(t, router, "/datasets?name=fremd", "user-a", germanCSV)
	if resp.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", resp.Code, resp.Body.String())
	}
	meta := repo.DatasetMeta{}
	if err := json.Unmarshal(resp.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}

	//a plain user does not see it
	if body := do(t, router, "GET", "/datasets", "user-b", nil).Body.String(); strings.Contains(body, meta.Id) {
		t.Errorf("a plain user must not see a foreign dataset: %s", body)
	}
	//the admin does, and may open it
	if body := doAsAdmin(t, router, "GET", "/datasets", "admin-1").Body.String(); !strings.Contains(body, meta.Id) {
		t.Errorf("an admin has to see every dataset, got %s", body)
	}
	if code := doAsAdmin(t, router, "GET", "/datasets/"+meta.Id, "admin-1").Code; code != http.StatusOK {
		t.Errorf("an admin has to be able to open a foreign dataset, got %d", code)
	}
	if code := doAsAdmin(t, router, "DELETE", "/datasets/"+meta.Id, "admin-1").Code; code != http.StatusNoContent {
		t.Errorf("an admin has to be able to delete a foreign dataset, got %d", code)
	}
}
