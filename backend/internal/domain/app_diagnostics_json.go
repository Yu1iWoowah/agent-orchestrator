package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"time"
)

// UnmarshalJSON keeps presence separate from legal zero values. Stored PR1
// metadata remains readable; the live validator requires every wire field.
func (s *AppDiagnosticsSnapshot) UnmarshalJSON(data []byte) error {
	type snapshotJSON AppDiagnosticsSnapshot
	var decoded snapshotJSON
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var wire any
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*s = AppDiagnosticsSnapshot(decoded)
	s.liveFieldsValidated = diagnosticJSONFieldsPresent(wire, reflect.TypeFor[AppDiagnosticsSnapshot](), true)
	return nil
}

// The JSON tags are the field inventory. Live roots require collector/identity
// despite their legacy omitempty tags; nested omitempty (node style) stays optional.
// Concrete types/enums/bounds and the conditional style requirement are checked
// by ValidAppDiagnosticsCaptureSnapshot after strict decoding.
func diagnosticJSONFieldsPresent(value any, typ reflect.Type, root bool) bool {
	if value == nil {
		return false
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ == reflect.TypeFor[time.Time]() {
			return true
		}
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}

		fields := make(map[string]reflect.StructField)
		for _, field := range reflect.VisibleFields(typ) {
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" || (field.Anonymous && name == "") {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field
		}
		for name, child := range object {
			field, known := fields[name]
			if !known || !diagnosticJSONFieldsPresent(child, field.Type, false) {
				return false
			}
		}
		for name, field := range fields {
			_, present := object[name]
			if !present && (root || !slices.Contains(strings.Split(field.Tag.Get("json"), ",")[1:], "omitempty")) {
				return false
			}
		}
	case reflect.Interface:
		// No fixed field inventory exists for an arbitrary object bag.
		return false
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok || typ.Key().Kind() != reflect.String {
			return false
		}
		for _, child := range object {
			if !diagnosticJSONFieldsPresent(child, typ.Elem(), false) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if !diagnosticJSONFieldsPresent(item, typ.Elem(), false) {
				return false
			}
		}
	}
	return true
}
