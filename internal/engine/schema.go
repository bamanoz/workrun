package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"time"
)

var evidenceSchemas sync.Map
var rawMessageType = reflect.TypeOf(json.RawMessage{})
var timeType = reflect.TypeOf(time.Time{})

func EvidenceSchema(action string) json.RawMessage {
	if cached, ok := evidenceSchemas.Load(action); ok {
		return cached.(json.RawMessage)
	}
	value, err := newEvidence(action)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(jsonSchema(reflect.TypeOf(value)))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	schema := json.RawMessage(raw)
	evidenceSchemas.Store(action, schema)
	return schema
}

func jsonSchema(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType {
		return map[string]any{}
	}
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	switch t.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		required := []string{}
		for i := range t.NumField() {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			parts := strings.Split(field.Tag.Get("json"), ",")
			name := parts[0]
			if name == "" {
				name = field.Name
			}
			if name == "-" {
				continue
			}
			properties[name] = jsonSchema(field.Type)
			if len(parts) == 1 || parts[1] != "omitempty" {
				required = append(required, name)
			}
		}
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": jsonSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": jsonSchema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}
