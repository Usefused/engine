package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
)

// inferSchema recursively walks the decoded JSON value and returns the type tree.
// Object keys are sorted at every level to ensure stable hashing.
func inferSchema(v any) map[string]any {
	// Use type switch to map Go types to JSON schema types
	switch val := v.(type) {
	case map[string]any:
		return inferObjectSchema(val)
	case []any:
		return inferArraySchema(val)
	case string:
		return map[string]any{"type": "string"}
	case float64:
		return map[string]any{"type": "number"}
	case bool:
		return map[string]any{"type": "boolean"}
	case nil:
		return map[string]any{"type": "null"}
	default:
		return map[string]any{"type": "unknown"}
	}
}

func inferObjectSchema(val map[string]any) map[string]any {
	props := make(map[string]any, len(val))
	keys := sortedKeys(val)
	// Iterate through sorted keys to ensure the properties map is built in a stable order
	for _, k := range keys {
		props[k] = inferSchema(val[k])
	}
	return map[string]any{"type": "object", "properties": props}
}

func inferArraySchema(val []any) map[string]any {
	// If the array is empty, the item type is unknown since there are no elements to inspect
	if len(val) == 0 {
		return map[string]any{"type": "array", "items": map[string]any{"type": "unknown"}}
	}
	// Iterate through the array to find the first valid element to infer the item type
	for _, item := range val {
		// Ignore null elements and use the first non-null element to infer the item type
		if item != nil {
			return map[string]any{"type": "array", "items": inferSchema(item)}
		}
	}
	// If all elements are null, fallback to declaring the item type as null
	return map[string]any{"type": "array", "items": map[string]any{"type": "null"}}
}

// sortedKeys returns a sorted list of keys from the map.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	// Iterate over the map to collect all keys before sorting
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaHash marshals the schema to JSON and returns its SHA-256 hash.
func schemaHash(schema map[string]any) string {
	b, _ := json.Marshal(schema)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// publishSchemaObservation infers the schema of the webhook payload and publishes it to NATS.
func publishSchemaObservation(body []byte, eventName string, config *webhookConfig) {
	var parsed any
	// If unmarshaling the payload fails, silently drop it as it is an invalid JSON payload
	if err := json.Unmarshal(body, &parsed); err != nil {
		return
	}

	obj, ok := parsed.(map[string]any)
	// Skip schema observation if the top-level structure is not a JSON object
	if !ok {
		slog.Warn("Schema observation skipped: top-level is not an object", slog.String("event", eventName))
		return
	}

	schema := inferSchema(obj)
	hash := schemaHash(schema)
	schemaJSON, _ := json.Marshal(schema)

	payload, _ := json.Marshal(map[string]any{
		"account_id":  config.AccountID,
		"service_id":  config.ServiceID,
		"event_name":  eventName,
		"schema_hash": hash,
		"schema_json": json.RawMessage(schemaJSON),
	})

	// Only publish the schema event if the NATS client is initialized
	if globalNATSClient != nil {
		globalNATSClient.PublishJS("webhook.schema.observed", payload)
	}
}
