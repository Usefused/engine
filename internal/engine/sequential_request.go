package engine

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strings"

	"github.com/Usefused/engine/internal/engine/streambody"
)

func buildSequentialRequestBody(content *SelectedRequestRepresentation, bodyParams map[string]any) (io.Reader, error) {
	name := strings.TrimSpace(content.PayloadParameter)
	if name == "" || len(bodyParams) != 1 {
		return nil, errors.New("sequential request requires one payload_parameter")
	}
	value, ok := bodyParams[name]
	if !ok {
		return nil, errors.New("sequential request payload is missing")
	}
	if reader, ok := value.(io.Reader); ok {
		// Native callers can provide an already-framed stream without Engine
		// buffering it; generated callers use the typed ordered-array path below.
		return reader, nil
	}
	items, err := positionalItems(value)
	if err != nil {
		return nil, errors.New("sequential request payload must be an ordered array or reader")
	}
	mediaType, _, _ := mime.ParseMediaType(content.MediaType)
	return streambody.New(func(destination io.Writer) error {
		return writeSequentialItems(destination, strings.ToLower(mediaType), items)
	}), nil
}

func writeSequentialItems(destination io.Writer, mediaType string, items []any) error {
	for _, item := range items {
		if err := writeSequentialItem(destination, mediaType, item); err != nil {
			return err
		}
	}
	return nil
}

func writeSequentialItem(destination io.Writer, mediaType string, item any) error {
	switch mediaType {
	case "application/jsonl", "application/x-ndjson":
		return json.NewEncoder(destination).Encode(item)
	case "application/json-seq":
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		if _, err := destination.Write([]byte{0x1e}); err != nil {
			return err
		}
		if _, err := destination.Write(payload); err != nil {
			return err
		}
		_, err = destination.Write([]byte{'\n'})
		return err
	default:
		return errors.New("sequential request media type is unsupported")
	}
}

func supportedSequentialJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "application/jsonl", "application/json-seq", "application/x-ndjson":
		return true
	default:
		return false
	}
}
