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

// uploadAsAdmin is upload with the admin realm role, which is what it takes for
// an administrator to own a dataset of their own.
func uploadAsAdmin(t *testing.T, router *gin.Engine, path string, userId string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	request.Header.Set("Authorization", adminTokenFor(userId))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func mustUpload(t *testing.T, response *httptest.ResponseRecorder) repo.DatasetMeta {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("setup failed: %d %s", response.Code, response.Body.String())
	}
	meta := repo.DatasetMeta{}
	if err := json.Unmarshal(response.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// routerWithTwoDatasetOwners stores one dataset for the plain user-a and one for
// the administrator admin-1, so a list can be checked for both.
func routerWithTwoDatasetOwners(t *testing.T) (router *gin.Engine, foreign repo.DatasetMeta, own repo.DatasetMeta) {
	t.Helper()
	router = datasetRouter(newFakeDatasets())
	foreign = mustUpload(t, upload(t, router, "/datasets?name=fremd", "user-a", germanCSV))
	own = mustUpload(t, uploadAsAdmin(t, router, "/datasets?name=eigen", "admin-1", germanCSV))
	return router, foreign, own
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

// ---------------------------------------------------------------------------
// An administrator sees every dataset only when asking for it
// ---------------------------------------------------------------------------

// The admin role used to widen the list by itself, which made a tool that looks
// a dataset up by name hit a foreign one of the same name.
func TestAnAdminListsOnlyTheirOwnDatasetsUnlessAllIsAskedFor(t *testing.T) {
	router, foreign, own := routerWithTwoDatasetOwners(t)

	//a plain user who owns nothing sees nothing, and an empty list, not null
	resp := do(t, router, "GET", "/datasets", "user-b", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if strings.TrimSpace(resp.Body.String()) != "[]" {
		t.Errorf("a caller without datasets has to get an empty list, got %s", resp.Body.String())
	}

	//and so does an admin who did not ask for more
	for _, path := range []string{"/datasets", "/datasets?all=false"} {
		resp = doAsAdmin(t, router, "GET", path, "admin-1")
		if resp.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d: %s", path, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), own.Id) {
			t.Errorf("%s: an admin has to see their own dataset, got %s", path, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), foreign.Id) {
			t.Errorf("%s: an admin must not see a foreign dataset without all=true, got %s", path, resp.Body.String())
		}
	}
}

func TestAnAdminListsEveryDatasetWithAllTrue(t *testing.T) {
	router, foreign, own := routerWithTwoDatasetOwners(t)

	//admin-2 owns nothing, so everything in the answer is somebody else's
	resp := doAsAdmin(t, router, "GET", "/datasets?all=true", "admin-2")
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), foreign.Id) || !strings.Contains(resp.Body.String(), own.Id) {
		t.Errorf("an admin asking for all has to see every dataset, got %s", resp.Body.String())
	}
}

func TestANonAdminAskingForEveryDatasetIsForbidden(t *testing.T) {
	router, foreign, _ := routerWithTwoDatasetOwners(t)

	//user-b owns nothing, so a widened list would be pure disclosure
	resp := do(t, router, "GET", "/datasets?all=true", "user-b", nil)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), foreign.Id) {
		t.Errorf("a refusal must not carry a foreign dataset: %s", resp.Body.String())
	}
	//the refusal is shared with the environments route, so it has to name which
	//list was asked for
	if !strings.Contains(resp.Body.String(), "every dataset") {
		t.Errorf("the refusal has to name what was asked for, got %s", resp.Body.String())
	}

	//all=false is not a claim to anything, so it is served like no parameter
	resp = do(t, router, "GET", "/datasets?all=false", "user-b", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), foreign.Id) {
		t.Errorf("a plain user must not see another user's dataset: %s", resp.Body.String())
	}
}

// The value is parsed before the role is looked at, so a non-admin sending
// nonsense is told what is wrong with the request rather than being refused.
func TestAnAllThatIsNoBooleanIsRejectedOnDatasets(t *testing.T) {
	router, foreign, own := routerWithTwoDatasetOwners(t)

	for _, query := range []string{"?all=maybe", "?all=", "?all=1.0"} {
		resp := do(t, router, "GET", "/datasets"+query, "user-a", nil)
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400 for a plain user, got %d: %s", query, resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), foreign.Id) {
			t.Errorf("%s: a refusal must not carry a dataset: %s", query, resp.Body.String())
		}
		resp = doAsAdmin(t, router, "GET", "/datasets"+query, "admin-1")
		if resp.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400 for an admin, got %d: %s", query, resp.Code, resp.Body.String())
		}
		if strings.Contains(resp.Body.String(), own.Id) {
			t.Errorf("%s: a refusal must not carry a dataset: %s", query, resp.Body.String())
		}
	}

	//strconv.ParseBool accepts these, and so must the route
	for _, query := range []string{"?all=1", "?all=TRUE", "?all=t"} {
		resp := doAsAdmin(t, router, "GET", "/datasets"+query, "admin-2")
		if resp.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d: %s", query, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), foreign.Id) {
			t.Errorf("%s: has to widen the list, got %s", query, resp.Body.String())
		}
		if resp := do(t, router, "GET", "/datasets"+query, "user-b", nil); resp.Code != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d: %s", query, resp.Code, resp.Body.String())
		}
	}
}

// The single dataset routes are untouched by the narrowed list: requireDataset
// still lets an administrator open and delete a foreign dataset.
func TestAnAdminOpensAndDeletesEveryDataset(t *testing.T) {
	router, foreign, _ := routerWithTwoDatasetOwners(t)

	if code := doAsAdmin(t, router, "GET", "/datasets/"+foreign.Id, "admin-1").Code; code != http.StatusOK {
		t.Errorf("an admin has to be able to open a foreign dataset, got %d", code)
	}
	if code := doAsAdmin(t, router, "DELETE", "/datasets/"+foreign.Id, "admin-1").Code; code != http.StatusNoContent {
		t.Errorf("an admin has to be able to delete a foreign dataset, got %d", code)
	}
}
