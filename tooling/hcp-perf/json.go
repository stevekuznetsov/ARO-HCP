// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
)

func decodeFile(path string, value any) error {
	if path == "" {
		return fmt.Errorf("file path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	input, err := readJSONValue(d, "$")
	if err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	// Decode the original bytes so integer precision and custom JSON types are preserved.
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	return requireJSONFields(input, reflect.TypeOf(value), "$")
}

// Token traversal catches duplicate keys before encoding/json can overwrite them.
// Null is never an input value; optional fields must be omitted instead.
func readJSONValue(d *json.Decoder, path string) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case nil:
		return nil, fmt.Errorf("%s: null is not allowed", path)
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("%s: expected an object key", path)
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("%s: duplicate JSON key %q", path, key)
			}
			value, err := readJSONValue(d, path+"."+key)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		_, err := d.Token()
		return object, err
	case json.Delim('['):
		var array []any
		for d.More() {
			value, err := readJSONValue(d, fmt.Sprintf("%s[%d]", path, len(array)))
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err := d.Token()
		return array, err
	default:
		return token, nil
	}
}

// The typed decoder validates shapes; this checks presence, which zero values lose.
func requireJSONFields(input any, typ reflect.Type, path string) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	// Types such as time.Time own their JSON representation, not their Go fields.
	if reflect.PointerTo(typ).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := input.(map[string]any)
		if !ok {
			return nil
		}
		known := make(map[string]bool, typ.NumField())
		var fields func(reflect.Type) error
		fields = func(typ reflect.Type) error {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				tag, tagged := field.Tag.Lookup("json")
				if !field.IsExported() || !tagged || tag == "-" {
					continue
				}
				parts := strings.Split(tag, ",")
				name := parts[0]
				if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
					if err := fields(field.Type); err != nil {
						return err
					}
					continue
				}
				if name == "" {
					name = field.Name
				}
				known[name] = true
				child, exists := object[name]
				if !exists {
					if slices.Contains(parts[1:], "omitempty") {
						continue
					}
					return fmt.Errorf("%s.%s: required field is missing", path, name)
				}
				if err := requireJSONFields(child, field.Type, path+"."+name); err != nil {
					return err
				}
			}
			return nil
		}
		if err := fields(typ); err != nil {
			return err
		}
		// encoding/json also accepts case-insensitive aliases, which could
		// overwrite an otherwise correctly spelled field in the same object.
		for key := range object {
			if !known[key] {
				return fmt.Errorf("%s: unknown or incorrectly cased field %q", path, key)
			}
		}
	case reflect.Slice, reflect.Array:
		array, _ := input.([]any)
		for i, child := range array {
			if err := requireJSONFields(child, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}
