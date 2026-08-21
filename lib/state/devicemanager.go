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

package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/SENERGY-Platform/moses/lib/util"
)

// The device-manager decides for itself what the caller may see, so its api is
// called with the caller's own token instead of a service account. token is the
// verbatim Authorization header value the caller sent, scheme included, and it
// is forwarded unchanged and never logged.
//
// These three helpers replace what lib/jwt offered as JwtImpersonate. The status
// handling is deliberately unchanged: only 200 counts as success, 401 becomes
// "access denied", every other status becomes an "unexpected statuscode" error.
// A device-manager answering 201 or 204 therefore still looks like a failure —
// whether that should change is a separate decision from swapping the token
// library, and the tests below pin it so the question stays visible.
//
// The device-repository client in an already present dependency was considered
// instead: it has no equivalent of the unparameterised GET /device-types this
// service relies on, it does not escape the id in GET /device-types/{id}, and it
// treats every 2xx as success. Each of those is a behaviour change, so the thin
// helper stays.

// errAccessDenied is returned when the device-manager rejects the caller's
// token. The text is the one the previous implementation used.
var errAccessDenied = errors.New("access denied")

// deviceManagerGetJson issues a GET and decodes the json response into result.
func deviceManagerGetJson(token string, endpoint string, result interface{}) error {
	response, err := deviceManagerRequest(token, http.MethodGet, endpoint, "", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return json.NewDecoder(response.Body).Decode(result)
}

// deviceManagerPostJson sends body as json and decodes the response into result,
// which may be nil when the response is not wanted.
func deviceManagerPostJson(token string, endpoint string, body interface{}, result interface{}) error {
	encoded := new(bytes.Buffer)
	// encoded before the request is built, so an unencodable body never reaches
	// the network
	err := json.NewEncoder(encoded).Encode(body)
	if err != nil {
		return err
	}
	response, err := deviceManagerRequest(token, http.MethodPost, endpoint, "application/json", encoded)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if result == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(result)
}

// deviceManagerDelete issues a DELETE and discards the response body.
func deviceManagerDelete(token string, endpoint string) error {
	response, err := deviceManagerRequest(token, http.MethodDelete, endpoint, "", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// drained so the connection can be reused
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

// deviceManagerRequest sends one request and hands the response back only for a
// 200; the caller then owns the body. Every other outcome closes the body here
// and returns an error, so no caller has to guess whether it owns a body it also
// got an error for — which is what the previous implementation left open.
func deviceManagerRequest(token string, method string, endpoint string, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", token)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		// the reason is only known downstream, so it is logged here; bounded,
		// because the body is not ours and an error page can be large
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		util.Logger.Error("device manager denied access", "method", method, "url", endpoint, "response", string(message))
		return nil, errAccessDenied
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("unexpected statuscode in response for %s %s", method, endpoint)
	}
	return response, nil
}
