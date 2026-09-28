package tama2026

import "encoding/json"

func mustRaw(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func objectSchema(additional bool, required []string, properties map[string]any) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": additional,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringField(description string, minLength, maxLength int) map[string]any {
	field := map[string]any{"type": "string"}
	if description != "" {
		field["description"] = description
	}
	if minLength > 0 {
		field["minLength"] = minLength
	}
	if maxLength > 0 {
		field["maxLength"] = maxLength
	}
	return field
}

func integerField(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func variantOutput(successRequired []string, successProps map[string]any) json.RawMessage {
	success := objectSchema(true, successRequired, successProps)
	failure := objectSchema(false, []string{"schema_version", "error"}, map[string]any{
		"schema_version": map[string]any{"type": "string"},
		"error":          map[string]any{"type": "object"},
	})
	return mustRaw(map[string]any{"anyOf": []any{success, failure}})
}

func stringProp() map[string]any { return map[string]any{"type": "string"} }

func objectProp() map[string]any { return map[string]any{"type": "object"} }

func objectArrayProp() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "object"}}
}

func stringArrayProp() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}

func boolProp() map[string]any { return map[string]any{"type": "boolean"} }

func nullableObject() map[string]any {
	return map[string]any{"anyOf": []any{
		map[string]any{"type": "object"},
		map[string]any{"type": "null"},
	}}
}

var readOnlyAnnotations = map[string]any{
	"readOnlyHint":    true,
	"destructiveHint": false,
	"idempotentHint":  true,
	"openWorldHint":   false,
}

var messageAnnotations = map[string]any{
	"readOnlyHint":    false,
	"destructiveHint": false,
	"idempotentHint":  false,
	"openWorldHint":   true,
}
