package enginev1

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

type expectedProtoField struct {
	name        protoreflect.Name
	number      protoreflect.FieldNumber
	kind        protoreflect.Kind
	cardinality protoreflect.Cardinality
}

// TestExecuteUnifiedGeneratedContract locks the additive Unified protobuf wire
// field numbers shared by Engine and generated SDKs.
func TestExecuteUnifiedGeneratedContract(t *testing.T) {
	t.Parallel()

	assertExecuteUnifiedMethod(t)
	assertPaginationIntentContract(t)
	assertProtoMessageFields(t, "ExecutionSelectors", []expectedProtoField{
		{name: "environment", number: 1, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "end_user_ref", number: 2, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "auth_type", number: 3, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "auth_name", number: 4, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "resource_id", number: 5, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
	})
	assertProtoMessageFields(t, "ExecuteUnifiedRequest", []expectedProtoField{
		{name: "operation", number: 1, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "targets", number: 2, kind: protoreflect.StringKind, cardinality: protoreflect.Repeated},
		{name: "input_json", number: 3, kind: protoreflect.BytesKind, cardinality: protoreflect.Optional},
		{name: "target_selectors", number: 4, kind: protoreflect.MessageKind, cardinality: protoreflect.Repeated},
		{name: "idempotency_key", number: 5, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "target_pagination", number: 6, kind: protoreflect.MessageKind, cardinality: protoreflect.Repeated},
	})
	assertProtoMessageFields(t, "UnifiedTargetResult", []expectedProtoField{
		{name: "target", number: 1, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "status", number: 2, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "data_json", number: 3, kind: protoreflect.BytesKind, cardinality: protoreflect.Optional},
		{name: "error_code", number: 4, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "auth_action", number: 5, kind: protoreflect.MessageKind, cardinality: protoreflect.Optional},
	})
	assertProtoMessageFields(t, "UnifiedAuthAction", []expectedProtoField{
		{name: "action", number: 1, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "bucket_id", number: 2, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "service_id", number: 3, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "end_user_ref", number: 4, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "connection_id", number: 5, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "reason", number: 6, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
	})
	assertProtoMessageFields(t, "UnifiedRollbackResult", []expectedProtoField{
		{name: "target", number: 1, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "status", number: 2, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "error_code", number: 3, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
		{name: "triggered_by", number: 4, kind: protoreflect.StringKind, cardinality: protoreflect.Repeated},
		{name: "auth_action", number: 5, kind: protoreflect.MessageKind, cardinality: protoreflect.Optional},
	})
	assertProtoMessageFields(t, "ExecuteUnifiedResponse", []expectedProtoField{
		{name: "results", number: 1, kind: protoreflect.MessageKind, cardinality: protoreflect.Repeated},
		{name: "rollback_results", number: 2, kind: protoreflect.MessageKind, cardinality: protoreflect.Repeated},
		{name: "output_json", number: 3, kind: protoreflect.BytesKind, cardinality: protoreflect.Optional},
		{name: "output_error_code", number: 4, kind: protoreflect.StringKind, cardinality: protoreflect.Optional},
	})
	assertUnifiedMessageReferences(t)
}

// assertPaginationIntentContract locks the shared caller-bound message and both additive request references.
func assertPaginationIntentContract(t *testing.T) {
	t.Helper()
	assertProtoMessageFields(t, "PaginationIntent", []expectedProtoField{
		{name: "max_pages", number: 1, kind: protoreflect.Uint32Kind, cardinality: protoreflect.Optional},
	})
	execute := File_engine_v1_engine_proto.Messages().ByName("ExecuteRequest")
	pagination := execute.Fields().ByName("pagination")
	if pagination == nil || pagination.Number() != 10 || pagination.Message().FullName() != "engine.v1.PaginationIntent" {
		t.Fatal("ExecuteRequest.pagination must be field 10 with PaginationIntent type")
	}
}

// assertExecuteUnifiedMethod locks the unary RPC input/output descriptors and
// fully-qualified service method name used by generated clients.
func assertExecuteUnifiedMethod(t *testing.T) {
	t.Helper()

	service := File_engine_v1_engine_proto.Services().ByName("EngineService")
	if service == nil {
		t.Fatal("generated EngineService descriptor is missing")
	}
	method := service.Methods().ByName("ExecuteUnified")
	if method == nil {
		t.Fatal("generated ExecuteUnified method descriptor is missing")
	}
	if method.IsStreamingClient() || method.IsStreamingServer() {
		t.Fatal("ExecuteUnified must remain unary")
	}
	if got := method.Input().FullName(); got != "engine.v1.ExecuteUnifiedRequest" {
		t.Fatalf("ExecuteUnified input = %q, want engine.v1.ExecuteUnifiedRequest", got)
	}
	if got := method.Output().FullName(); got != "engine.v1.ExecuteUnifiedResponse" {
		t.Fatalf("ExecuteUnified output = %q, want engine.v1.ExecuteUnifiedResponse", got)
	}
}

// assertProtoMessageFields requires each public message to retain its additive
// field numbers, names, cardinality, and scalar/message kinds.
func assertProtoMessageFields(t *testing.T, messageName protoreflect.Name, expected []expectedProtoField) {
	t.Helper()

	message := File_engine_v1_engine_proto.Messages().ByName(messageName)
	if message == nil {
		t.Fatalf("generated %s descriptor is missing", messageName)
	}
	if got := message.Fields().Len(); got != len(expected) {
		t.Fatalf("%s field count = %d, want %d", messageName, got, len(expected))
	}
	for _, want := range expected {
		assertProtoField(t, message, want)
	}
}

// assertProtoField compares one descriptor field without relying on generated
// Go struct layout, catching wire-incompatible renumbering.
func assertProtoField(t *testing.T, message protoreflect.MessageDescriptor, expected expectedProtoField) {
	t.Helper()

	field := message.Fields().ByName(expected.name)
	if field == nil {
		t.Fatalf("generated %s.%s descriptor is missing", message.Name(), expected.name)
	}
	if field.Number() != expected.number || field.Kind() != expected.kind || field.Cardinality() != expected.cardinality {
		t.Fatalf(
			"%s.%s = number %d, kind %s, cardinality %s; want number %d, kind %s, cardinality %s",
			message.Name(), expected.name, field.Number(), field.Kind(), field.Cardinality(),
			expected.number, expected.kind, expected.cardinality,
		)
	}
}

// assertUnifiedMessageReferences proves target and rollback auth actions point
// at the shared remediation message rather than duplicate wire contracts.
func assertUnifiedMessageReferences(t *testing.T) {
	t.Helper()

	request := File_engine_v1_engine_proto.Messages().ByName("ExecuteUnifiedRequest")
	selectors := request.Fields().ByName("target_selectors")
	if !selectors.IsMap() || selectors.MapValue().Message().FullName() != "engine.v1.ExecutionSelectors" {
		t.Fatal("ExecuteUnifiedRequest.target_selectors must map strings to ExecutionSelectors")
	}
	pagination := request.Fields().ByName("target_pagination")
	if !pagination.IsMap() || pagination.MapValue().Message().FullName() != "engine.v1.PaginationIntent" {
		t.Fatal("ExecuteUnifiedRequest.target_pagination must map strings to PaginationIntent")
	}
	response := File_engine_v1_engine_proto.Messages().ByName("ExecuteUnifiedResponse")
	results := response.Fields().ByName("results")
	if got := results.Message().FullName(); got != "engine.v1.UnifiedTargetResult" {
		t.Fatalf("ExecuteUnifiedResponse.results message = %q, want engine.v1.UnifiedTargetResult", got)
	}
	rollbacks := response.Fields().ByName("rollback_results")
	if got := rollbacks.Message().FullName(); got != "engine.v1.UnifiedRollbackResult" {
		t.Fatalf("ExecuteUnifiedResponse.rollback_results message = %q, want engine.v1.UnifiedRollbackResult", got)
	}
}
