package sandbox

import (
	"encoding/json"
	"testing"
)

func TestInferSchemaAndHash(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		expectedJSON string
	}{
		{
			name: "flat object",
			payload: `{
				"userId": "abc123",
				"amount": 42.5,
				"paid": true,
				"nullable": null
			}`,
			expectedJSON: `{"properties":{"amount":{"type":"number"},"nullable":{"type":"null"},"paid":{"type":"boolean"},"userId":{"type":"string"}},"type":"object"}`,
		},
		{
			name: "nested object with array",
			payload: `{
				"data": {
					"items": [1, 2, 3],
					"tags": ["vip", "trial"]
				}
			}`,
			expectedJSON: `{"properties":{"data":{"properties":{"items":{"items":{"type":"number"},"type":"array"},"tags":{"items":{"type":"string"},"type":"array"}},"type":"object"}},"type":"object"}`,
		},
		{
			name: "array with null first element",
			payload: `{
				"list": [null, "string2"]
			}`,
			expectedJSON: `{"properties":{"list":{"items":{"type":"string"},"type":"array"}},"type":"object"}`,
		},
		{
			name: "empty array",
			payload: `{
				"empty": []
			}`,
			expectedJSON: `{"properties":{"empty":{"items":{"type":"unknown"},"type":"array"}},"type":"object"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parsed any
			err := json.Unmarshal([]byte(tt.payload), &parsed)
			if err != nil {
				t.Fatalf("Failed to parse test payload: %v", err)
			}

			schema := inferSchema(parsed)
			schemaJSON, err := json.Marshal(schema)
			if err != nil {
				t.Fatalf("Failed to marshal schema: %v", err)
			}

			if string(schemaJSON) != tt.expectedJSON {
				t.Errorf("Expected schema JSON: %s, got: %s", tt.expectedJSON, string(schemaJSON))
			}
		})
	}
}

func TestStableHashing(t *testing.T) {
	// Two objects with the same keys in different order
	payload1 := `{"a": 1, "b": 2, "c": 3}`
	payload2 := `{"c": 3, "a": 1, "b": 2}`

	var parsed1, parsed2 any
	json.Unmarshal([]byte(payload1), &parsed1)
	json.Unmarshal([]byte(payload2), &parsed2)

	schema1 := inferSchema(parsed1)
	schema2 := inferSchema(parsed2)

	hash1 := schemaHash(schema1)
	hash2 := schemaHash(schema2)

	if hash1 != hash2 {
		t.Errorf("Expected stable hashing for reordered keys. Hash1: %s, Hash2: %s", hash1, hash2)
	}

	schemaJSON1, _ := json.Marshal(schema1)
	schemaJSON2, _ := json.Marshal(schema2)

	if string(schemaJSON1) != string(schemaJSON2) {
		t.Errorf("Expected identical schema JSON strings, got %s and %s", string(schemaJSON1), string(schemaJSON2))
	}
}
