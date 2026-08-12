package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Decode keeps execution contracts fail-closed: accepting an unknown field can
// silently turn a reviewed policy into a weaker zero-value policy.
func Decode(data []byte, target any, contract string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s contains trailing data", contract)
	}
	return nil
}
