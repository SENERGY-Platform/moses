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

type Service struct {
	Id          string           `json:"id,omitempty" bson:"id"`
	Name        string           `json:"name,omitempty" bson:"name"`
	Description string           `json:"description,omitempty" bson:"description"`
	Input       []TypeAssignment `json:"input,omitempty" bson:"input"`
	Output      []TypeAssignment `json:"output,omitempty" bson:"output"` // list of alternative result types; for example a string if success or a json on error
	Protocol    Protocol         `json:"protocol,omitempty" bson:"-"`
}

type Protocol struct {
	Id                 string       `json:"id"`
	ProtocolHandlerUrl string       `json:"protocol_handler_url"`
	Name               string       `json:"name"`
	Desc               string       `json:"description"`
	MsgStructure       []MsgSegment `json:"msg_structure"`
}

type TypeAssignment struct {
	Name       string     `json:"name" bson:"name"`
	Type       ValueType  `json:"type" bson:"type"`
	MsgSegment MsgSegment `json:"msg_segment" bson:"msg_segment"`
}

type MsgSegment struct {
	Id   string `json:"id" bson:"id"`
	Name string `json:"name" bson:"name"`
}

type FieldType struct {
	Id   string    `json:"id,omitempty" bson:"id"`
	Name string    `json:"name,omitempty" bson:"name"`
	Type ValueType `json:"type,omitempty" bson:"type"`
}

type ValueType struct {
	Id          string      `json:"id,omitempty" bson:"id"`
	Name        string      `json:"name,omitempty" bson:"name"`
	Description string      `json:"description,omitempty" bson:"description"`
	BaseType    string      `json:"base_type,omitempty" bson:"base_type"`
	Fields      []FieldType `json:"fields" bson:"fields"`
	Literal     string      `json:"literal" bson:"literal"` //is literal, if not empty
}

const (
	IndexStructBaseType = "http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#index_structure"
	StructBaseType      = "http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#structure"
	MapBaseType         = "http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#map"
	ListBaseType        = "http://www.sepl.wifa.uni-leipzig.de/ontlogies/device-repo#list"

	XsdString = "http://www.w3.org/2001/XMLSchema#string"
	XsdInt    = "http://www.w3.org/2001/XMLSchema#integer"
	XsdFloat  = "http://www.w3.org/2001/XMLSchema#decimal"
	XsdBool   = "http://www.w3.org/2001/XMLSchema#boolean"
)

type AllowedValues struct {
	Primitive   []string `json:"primitive"`
	Collections []string `json:"collections"`
	Structures  []string `json:"structures"`
	Map         []string `json:"map"`
	Set         []string `json:"set"`
}

func GetAllowedValuesBase() AllowedValues {
	return AllowedValues{
		Map: []string{
			MapBaseType,
		},
		Set: []string{
			ListBaseType,
		},
		Collections: []string{
			ListBaseType,
			MapBaseType,
		},
		Structures: []string{
			IndexStructBaseType,
			StructBaseType,
		},
		Primitive: []string{
			XsdString,
			XsdInt,
			XsdFloat,
			XsdBool,
		},
	}
}

func (allowedValues AllowedValues) IsMap(valueType ValueType) bool {
	for _, element := range allowedValues.Map {
		if element == valueType.BaseType {
			return true
		}
	}
	return false
}

func (allowedValues AllowedValues) IsSet(valueType ValueType) bool {
	for _, element := range allowedValues.Set {
		if element == valueType.BaseType {
			return true
		}
	}
	return false
}

func (allowedValues AllowedValues) IsCollection(valueType ValueType) bool {
	for _, element := range allowedValues.Collections {
		if element == valueType.BaseType {
			return true
		}
	}
	return false
}

func (allowedValues AllowedValues) IsStructure(valueType ValueType) bool {
	for _, element := range allowedValues.Structures {
		if element == valueType.BaseType {
			return true
		}
	}
	return false
}

func (allowedValues AllowedValues) IsPrimitive(valueType ValueType) bool {
	for _, element := range allowedValues.Primitive {
		if element == valueType.BaseType {
			return true
		}
	}
	return false
}
