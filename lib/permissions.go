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

package lib

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SENERGY-Platform/moses/lib/api"
	permModel "github.com/SENERGY-Platform/permissions-v2/pkg/model"
)

// permissionsTimeout bounds one call. A share touches every device of an
// environment twice, and the api's write timeout is ten seconds, so a
// permissions-v2 that accepts connections and never answers must not be able to
// hold the whole request.
const permissionsTimeout = 5 * time.Second

// maxPermissionsErrorBytes bounds how much of an error body is quoted back. It
// travels into the 502 of a share, so it has to be a reason and not a page.
const maxPermissionsErrorBytes = 512

// permissionsClient speaks the two endpoints of permissions-v2 a share needs. It
// exists because the module's own client does not pass the context on to the
// request and has no timeout, so nothing would ever end a hanging call.
type permissionsClient struct {
	url    string
	client *http.Client
}

var _ api.Permissions = &permissionsClient{}

func newPermissionsClient(serverUrl string) *permissionsClient {
	return &permissionsClient{
		url:    strings.TrimSuffix(serverUrl, "/"),
		client: &http.Client{Timeout: permissionsTimeout},
	}
}

func (this *permissionsClient) GetResource(ctx context.Context, token string, topicId string, id string) (permModel.Resource, error, int) {
	result := permModel.Resource{}
	err, code := this.do(ctx, http.MethodGet, token, this.resourceUrl(topicId, id), nil, &result)
	return result, err, code
}

func (this *permissionsClient) SetPermission(ctx context.Context, token string, topicId string, id string, rights permModel.ResourcePermissions) (permModel.ResourcePermissions, error, int) {
	result := permModel.ResourcePermissions{}
	err, code := this.do(ctx, http.MethodPut, token, this.resourceUrl(topicId, id), rights, &result)
	return result, err, code
}

// resourceUrl carries the client version permissions-v2 checks: if it ever
// serves a breaking version, this gets 426 instead of talking the wrong
// protocol to it.
func (this *permissionsClient) resourceUrl(topicId string, id string) string {
	return fmt.Sprintf("%v/manage/%v/%v?version=%v",
		this.url, url.PathEscape(topicId), url.PathEscape(id), url.QueryEscape(permModel.ClientVersion))
}

// do runs one request. The token goes into the header and nowhere else: it is
// the caller's credential, so it must not reach a log line or an error message.
func (this *permissionsClient) do(ctx context.Context, method string, token string, requestUrl string, body interface{}, result interface{}) (error, int) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err, 0
		}
		payload = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, requestUrl, payload)
	if err != nil {
		return err, 0
	}
	request.Header.Set("Authorization", token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := this.client.Do(request)
	if err != nil {
		//the url is quoted, the token is not
		return fmt.Errorf("unable to reach permissions-v2: %w", err), 0
	}
	defer response.Body.Close()
	if response.StatusCode > 299 {
		//the message of the refusal is what a caller needs: permissions-v2
		//answers a group somebody may not share with by explaining exactly that
		reason, _ := io.ReadAll(io.LimitReader(response.Body, maxPermissionsErrorBytes))
		return fmt.Errorf("permissions-v2 answered %v: %v", response.StatusCode, strings.TrimSpace(string(reason))), response.StatusCode
	}
	if err = json.NewDecoder(response.Body).Decode(result); err != nil {
		return fmt.Errorf("unable to read the answer of permissions-v2: %w", err), response.StatusCode
	}
	return nil, response.StatusCode
}
