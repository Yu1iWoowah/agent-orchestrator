package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// UnmarshalJSON rejects duplicate members and root aliases before encoding/json
// can silently combine them. Config leaves are checked against ProjectConfig
// at the service boundary, before expected comparisons and typed decoding.
func (in *PatchSettingsInput) UnmarshalJSON(data []byte) error {
	value, err := readSettingsJSON(json.NewDecoder(bytes.NewReader(data)))
	if err != nil {
		return err
	}
	if err := validateSettingsKeys(value, reflect.TypeFor[PatchSettingsInput](), "settings"); err != nil {
		return err
	}
	type wire PatchSettingsInput
	return json.Unmarshal(data, (*wire)(in))
}

func readSettingsJSON(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			member, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := member.(string)
			if !ok {
				return nil, fmt.Errorf("invalid settings JSON member")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate settings JSON member %q", key)
			}
			object[key], err = readSettingsJSON(decoder)
			if err != nil {
				return nil, err
			}
		}
		_, err := decoder.Token()
		return object, err
	case '[':
		values := []any{}
		for decoder.More() {
			value, err := readSettingsJSON(decoder)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		_, err := decoder.Token()
		return values, err
	default:
		return nil, fmt.Errorf("invalid settings JSON delimiter %q", delimiter)
	}
}

func validateSettingsKeys(value any, schema reflect.Type, path string) error {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	switch members := value.(type) {
	case map[string]any:
		if schema.Kind() != reflect.Map && schema.Kind() != reflect.Struct {
			return nil
		}
		for key, member := range members {
			var memberType reflect.Type
			if schema.Kind() == reflect.Map {
				memberType = schema.Elem()
			} else {
				for i := range schema.NumField() {
					field := schema.Field(i)
					name := strings.Split(field.Tag.Get("json"), ",")[0]
					if name == "" {
						name = field.Name
					}
					if field.IsExported() && name != "-" && key == name {
						memberType = field.Type
						break
					}
				}
			}
			if memberType == nil {
				return fmt.Errorf("unknown settings JSON member %q", path+"."+key)
			}
			if err := validateSettingsKeys(member, memberType, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		if schema.Kind() == reflect.Slice || schema.Kind() == reflect.Array {
			for _, member := range members {
				if err := validateSettingsKeys(member, schema.Elem(), path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
