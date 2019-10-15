/*
 * Copyright 2019 InfAI (CC SES)
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

package jwt

import (
	"net/http"

	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"log"

	"reflect"

	"io"

	"bytes"
)

type Jwt struct {
	UserId         string                 `json:"sub"`
	ResourceAccess map[string]Resource    `json:"resource_access"`
	RealmAccess    Resource               `json:"realm_access"`
	Map            map[string]interface{} `json:"-"`
	Impersonate    JwtImpersonate         `json:"-"`
}

type JwtImpersonate string

type Resource struct {
	Roles []string `json:"roles"`
}

func GetJwt(r *http.Request) (token Jwt, err error) {
	token.Map = map[string]interface{}{}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		err = errors.New("missing Authorization header")
		return
	}
	err = getJwtPayload(auth, &token.Map, &token)
	if err != nil {
		log.Println("error in getJwtPayload() ", err)
		return
	}
	token.Impersonate = JwtImpersonate(auth)
	return
}

func getJwtPayload(auth string, results ...interface{}) (err error) {
	authParts := strings.Split(auth, " ")
	if len(authParts) != 2 {
		return errors.New("expect auth string format like '<type> <token>'")
	}
	tokenString := authParts[1]
	tokenParts := strings.Split(tokenString, ".")
	if len(tokenParts) != 3 {
		return errors.New("expect token string format like '<head>.<payload>.<sig>'")
	}
	payloadSegment := tokenParts[1]
	err = decodeJwtSegment(payloadSegment, results...)
	return
}

// Decode JWT specific base64url encoding with padding stripped
func decodeJwtSegment(seg string, results ...interface{}) error {
	if l := len(seg) % 4; l > 0 {
		seg += strings.Repeat("=", 4-l)
	}

	b, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		log.Println("error while base64.URLEncoding.DecodeString()", err, seg)
		return err
	}

	for _, result := range results {
		err = json.Unmarshal(b, result)
		if err != nil {
			log.Println("error while json.Unmarshal()", err, reflect.TypeOf(result).Kind().String(), string(b))
			return err
		}
	}

	return nil
}

func (this JwtImpersonate) Post(url string, contentType string, body io.Reader) (resp *http.Response, err error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", string(this))
	req.Header.Set("Content-Type", contentType)

	resp, err = http.DefaultClient.Do(req)

	if err == nil && resp.StatusCode == 401 {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		resp.Body.Close()
		log.Println(buf.String())
		err = errors.New("access denied")
	}
	if err == nil && (resp.StatusCode != 200) {
		err = errors.New("unexpected statuscode in response for POST " + url)
	}
	return
}

func (this JwtImpersonate) PostJSON(url string, body interface{}, result interface{}) (err error) {
	b := new(bytes.Buffer)
	err = json.NewEncoder(b).Encode(body)
	if err != nil {
		return
	}
	resp, err := this.Post(url, "application/json", b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if result != nil {
		err = json.NewDecoder(resp.Body).Decode(result)
	}
	return
}

func (this JwtImpersonate) Get(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", string(this))
	resp, err = http.DefaultClient.Do(req)

	if err == nil && resp.StatusCode == 401 {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		resp.Body.Close()
		log.Println(buf.String())
		err = errors.New("access denied")
	}
	if err == nil && (resp.StatusCode != 200) {
		err = errors.New("unexpected statuscode in response for POST " + url)
	}
	return
}

func (this JwtImpersonate) Delete(url string) (resp *http.Response, err error) {
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", string(this))
	resp, err = http.DefaultClient.Do(req)

	if err == nil && resp.StatusCode == 401 {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		resp.Body.Close()
		log.Println(buf.String())
		err = errors.New("access denied")
	}
	if err == nil && (resp.StatusCode != 200) {
		err = errors.New("unexpected statuscode in response for POST " + url)
	}
	return
}

func (this JwtImpersonate) GetJSON(url string, result interface{}) (err error) {
	resp, err := this.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(result)
}
