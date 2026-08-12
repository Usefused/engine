package engine

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func TestPositionalMultipartDefaultsComeFromItemSchema(t *testing.T) {
	tests := []struct {
		name, schemaType, format, contentType, body string
		value                                       any
	}{
		{name: "scalar", schemaType: "integer", contentType: "text/plain", body: "42", value: 42},
		{name: "binary", schemaType: "string", format: "binary", contentType: "application/octet-stream", body: "raw", value: []byte("raw")},
		{name: "composite", schemaType: "object", contentType: "application/json", body: "{\"id\":1}\n", value: map[string]any{"id": 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := positionalTestContent("multipart/mixed", "items", test.schemaType, test.format, models.RequestEncoding{})
			part := readSinglePositionalPart(t, content, []any{test.value})
			if part.Header.Get("Content-Type") != test.contentType || string(part.Body) != test.body {
				t.Fatalf("headers=%#v payload=%q", part.Header, part.Body)
			}
		})
	}
}

func TestPositionalMultipartExplicitMediaOverridesRuntimeShape(t *testing.T) {
	encoding := models.RequestEncoding{ContentType: "application/json"}
	content := positionalTestContent("multipart/mixed", "items", "string", "", encoding)
	part := readSinglePositionalPart(t, content, []any{"value"})
	if part.Header.Get("Content-Type") != "application/json" || string(part.Body) != "\"value\"\n" {
		t.Fatalf("headers=%#v payload=%q", part.Header, part.Body)
	}
}

func TestPositionalFormDataUsesRepeatedContractName(t *testing.T) {
	content := positionalTestContent("multipart/form-data", "files", "string", "binary", models.RequestEncoding{})
	parts := readPositionalParts(t, content, []any{[]byte("one"), []byte("two")})
	for index, part := range parts {
		if positionalPartName(part.Header) != "files" || part.Header.Get("Content-Disposition") != "form-data; name=files" {
			t.Fatalf("part %d disposition = %q", index, part.Header.Get("Content-Disposition"))
		}
	}
}

func TestPositionalFormDataPreservesValidDispositionAndCustomHeader(t *testing.T) {
	encoding := models.RequestEncoding{ContentType: "text/plain", Headers: map[string]models.HeaderContract{
		"Content-Disposition": {Example: `form-data; name="files"; filename="reviewed.txt"`},
		"X-Part-Kind":         {Example: "reviewed"},
	}}
	content := positionalTestContent("multipart/form-data", "files", "string", "", encoding)
	part := readSinglePositionalPart(t, content, []any{"hello"})
	if part.Header.Get("Content-Disposition") != `form-data; name="files"; filename="reviewed.txt"` || part.Header.Get("X-Part-Kind") != "reviewed" {
		t.Fatalf("headers = %#v", part.Header)
	}
}

func TestPositionalFormDataRejectsInvalidOrMismatchedName(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		disposition string
	}{
		{name: "invalid payload", payload: "files\r\nX-Evil"},
		{name: "mismatched explicit disposition", payload: "files", disposition: "form-data; name=other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoding := models.RequestEncoding{}
			if test.disposition != "" {
				encoding.Headers = map[string]models.HeaderContract{"Content-Disposition": {Example: test.disposition}}
			}
			content := positionalTestContent("multipart/form-data", test.payload, "string", "", encoding)
			headers := map[string]string{}
			body, err := buildPositionalMultipartRequestBody(content, headers, map[string]any{test.payload: []any{"value"}})
			if err == nil {
				_, err = io.ReadAll(body)
				_ = body.(io.Closer).Close()
			}
			if err == nil {
				t.Fatal("invalid form-data name was accepted")
			}
		})
	}
}

func TestNestedPositionalMultipartDispositionFollowsEachFraming(t *testing.T) {
	tests := []struct {
		name, outer, nested    string
		outerNamed, childNamed bool
	}{
		{name: "form data around mixed", outer: "multipart/form-data", nested: "multipart/mixed", outerNamed: true},
		{name: "mixed around form data", outer: "multipart/mixed", nested: "multipart/form-data", childNamed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nested := models.RequestEncoding{ContentType: test.nested, ItemEncoding: &models.RequestEncoding{}}
			content := positionalTestContent(test.outer, "payload", "array", "", nested)
			content.ItemSchema.Projection.Items = &models.Schema{Type: "string"}
			outerPart := readSinglePositionalPart(t, content, []any{[]any{"one", "two"}})
			assertPartName(t, outerPart, test.outerNamed)
			_, parameters, err := mime.ParseMediaType(outerPart.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			child, err := multipart.NewReader(bytes.NewReader(outerPart.Body), parameters["boundary"]).NextPart()
			if err != nil {
				t.Fatal(err)
			}
			assertPartName(t, capturedPart{Header: child.Header}, test.childNamed)
			if child.Header.Get("Content-Type") != "text/plain" {
				t.Fatalf("nested child content type = %q", child.Header.Get("Content-Type"))
			}
		})
	}
}

func positionalTestContent(mediaType, payloadName, schemaType, format string, encoding models.RequestEncoding) *SelectedRequestRepresentation {
	return &SelectedRequestRepresentation{MediaType: mediaType, Serialization: models.RequestSerializationMultipart, PayloadParameter: payloadName,
		ItemSchema: &models.SchemaContract{Projection: models.Schema{Type: schemaType, Format: format}}, ItemEncoding: &encoding}
}

type capturedPart struct {
	Header textproto.MIMEHeader
	Body   []byte
}

func readSinglePositionalPart(t *testing.T, content *SelectedRequestRepresentation, values []any) capturedPart {
	t.Helper()
	parts := readPositionalParts(t, content, values)
	if len(parts) != 1 {
		t.Fatalf("part count = %d", len(parts))
	}
	return parts[0]
}

func readPositionalParts(t *testing.T, content *SelectedRequestRepresentation, values []any) []capturedPart {
	t.Helper()
	headers := map[string]string{}
	body, err := buildPositionalMultipartRequestBody(content, headers, map[string]any{content.PayloadParameter: values})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = body.(io.Closer).Close() })
	_, parameters, err := mime.ParseMediaType(headers["Content-Type"])
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(body, parameters["boundary"])
	parts := make([]capturedPart, 0, len(values))
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			return parts
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		payload, readErr := io.ReadAll(part)
		if readErr != nil {
			t.Fatal(readErr)
		}
		parts = append(parts, capturedPart{Header: cloneMIMEHeader(part.Header), Body: payload})
	}
}

func cloneMIMEHeader(header textproto.MIMEHeader) textproto.MIMEHeader {
	clone := make(textproto.MIMEHeader, len(header))
	for name, values := range header {
		clone[name] = append([]string(nil), values...)
	}
	return clone
}

func assertPartName(t *testing.T, part capturedPart, named bool) {
	t.Helper()
	if (positionalPartName(part.Header) == "payload") != named {
		t.Fatalf("Content-Disposition = %q", part.Header.Get("Content-Disposition"))
	}
}

func positionalPartName(header textproto.MIMEHeader) string {
	_, parameters, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	return parameters["name"]
}

type positionalBlockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (r *positionalBlockingReadCloser) Read([]byte) (int, error) {
	<-r.closed
	return 0, errors.New("source closed")
}

func (r *positionalBlockingReadCloser) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestPositionalMultipartCloseOwnsNestedReader(t *testing.T) {
	source := &positionalBlockingReadCloser{closed: make(chan struct{})}
	content := positionalTestContent("multipart/mixed", "items", "string", "binary", models.RequestEncoding{})
	body, err := buildPositionalMultipartRequestBody(content, map[string]string{}, map[string]any{"items": []any{source}})
	if err != nil {
		t.Fatal(err)
	}
	if err := body.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-source.closed:
	default:
		t.Fatal("nested positional source remained open")
	}
}
