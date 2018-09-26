/*
 * Copyright 2018 SENERGY Team
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

package marshaller

type InputOutput struct {
	Name    string        `json:"name,omitempty"`
	FieldId string        `json:"field_id"`
	Type    Type          `json:"type"`
	Value   string        `json:"value,omitempty"`
	Values  []InputOutput `json:"values,omitempty"`
}

type Type struct {
	Id   string `json:"id"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	Base string `json:"base"`
}

type FormatedOutput map[string]interface{}
