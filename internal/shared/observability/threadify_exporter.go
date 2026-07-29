package observability

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	threadify "github.com/ThreadifyDev/go-sdk"
)

// ThreadifyExporter translates OpenTelemetry spans into Threadify threads and steps.
// This is a custom fork of the official Threadify exporter to specifically handle
// "fused.*" attribute prefixes and extract error messages from exception events.
type ThreadifyExporter struct {
	conn    *threadify.Connection
	options SpanExporterOptions

	mu             sync.Mutex
	traceThreadMap map[string]*threadify.ThreadInstance
}

type SpanExporterOptions struct {
	Refs    []string
	Filters []string
}

// NewThreadifyExporter creates a new OpenTelemetry exporter for Threadify.
func NewThreadifyExporter(conn *threadify.Connection, opts SpanExporterOptions) *ThreadifyExporter {
	if opts.Refs == nil {
		opts.Refs = []string{}
	}
	if opts.Filters == nil {
		opts.Filters = []string{}
	}
	return &ThreadifyExporter{
		conn:           conn,
		options:        opts,
		traceThreadMap: make(map[string]*threadify.ThreadInstance),
	}
}

// ExportSpans converts spans to Threadify events.
func (e *ThreadifyExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if !e.conn.IsConnected() {
		return fmt.Errorf("threadify connection is not open")
	}

	for _, span := range spans {
		if e.shouldDrop(span.Name()) {
			continue
		}
		if err := e.processSpan(ctx, span); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown does nothing for now as the connection handles its own closure.
func (e *ThreadifyExporter) Shutdown(ctx context.Context) error {
	return nil
}

func (e *ThreadifyExporter) processSpan(ctx context.Context, span sdktrace.ReadOnlySpan) error {
	thread, err := e.getOrStartThread(ctx, span)
	if err != nil {
		return err
	}

	stepName := attrString(span.Attributes(), "threadify.step_name")
	if stepName == "" {
		stepName = span.Name()
	}

	step := thread.Step(stepName)

	contextData := make(map[string]any)
	refs := make(map[string]string)

	refKeys := refKeySet(e.options.Refs)
	skipKeys := map[string]bool{
		"threadify.thread_id": true,
		"threadify.contract":  true,
		"threadify.label":     true,
		"threadify.step_name": true,
		"threadify.role":      true,
		"threadify.service":   true,
		"threadify.tags":      true,
		"fused.tags":          true, // Ensure we skip the fused tags key, it's mapped in getOrStartThread
	}

	for _, attr := range span.Attributes() {
		key := string(attr.Key)
		if skipKeys[key] {
			continue
		}

		if refKeys[key] || hasPrefix(key, "threadify.ref.") || hasPrefix(key, "fused.ref.") {
			refKey := key
			if hasPrefix(key, "threadify.ref.") {
				refKey = key[len("threadify.ref."):]
			} else if hasPrefix(key, "fused.ref.") {
				refKey = key[len("fused.ref."):]
			}
			refs[refKey] = attrValueToString(attr.Value)
		} else if hasPrefix(key, "threadify.context.") || hasPrefix(key, "fused.ctx.") {
			ctxKey := key
			if hasPrefix(key, "threadify.context.") {
				ctxKey = key[len("threadify.context."):]
			} else if hasPrefix(key, "fused.ctx.") {
				ctxKey = key[len("fused.ctx."):]
			}
			contextData[ctxKey] = attrValueToAny(attr.Value)
		} else {
			contextData[key] = attrValueToAny(attr.Value)
		}
	}

	// Add times to context since there is no direct timestamp injection in exported API
	if !span.StartTime().IsZero() {
		contextData["otel.start"] = span.StartTime().UTC().Format(time.RFC3339Nano)
	}
	if !span.EndTime().IsZero() {
		contextData["otel.end"] = span.EndTime().UTC().Format(time.RFC3339Nano)
	}

	if len(contextData) > 0 {
		step.AddContext(contextData)
	}
	if len(refs) > 0 {
		if err := thread.AddRefs(ctx, refs); err != nil {
			return fmt.Errorf("add span refs: %w", err)
		}
	}

	for _, evt := range span.Events() {
		payload := make(map[string]any, len(evt.Attributes))
		for _, attr := range evt.Attributes {
			payload[string(attr.Key)] = attrValueToAny(attr.Value)
		}
		step.SubStep(evt.Name, payload)
	}

	statusCode := span.Status().Code
	var targetStatus string
	var message string

	switch {
	case statusCode == 2: // Error
		targetStatus = "failed"
		message = span.Status().Description
		if message == "" {
			// Extract exception message from auto-instrumented OTEL exception events
			for _, evt := range span.Events() {
				if evt.Name == "exception" {
					message = attrString(evt.Attributes, "exception.message")
					if message == "" {
						message = attrString(evt.Attributes, "exception.type")
					}
					break
				}
			}
			if message == "" {
				message = "Span ended with error status"
			}
		}
	default:
		targetStatus = "success"
		message = span.Status().Description
	}

	var resultErr error
	if targetStatus == "success" {
		_, resultErr = step.Success(ctx, message)
	} else {
		_, resultErr = step.Failed(ctx, message)
	}

	parentSpanID := span.Parent().SpanID()
	if !parentSpanID.IsValid() {
		if targetStatus == "success" {
			_, _ = thread.Complete(ctx, "Root span completed successfully")
		} else {
			_, _ = thread.Cancel(ctx, message)
		}
		e.mu.Lock()
		delete(e.traceThreadMap, span.SpanContext().TraceID().String())
		e.mu.Unlock()
	}

	return resultErr
}

func (e *ThreadifyExporter) getOrStartThread(ctx context.Context, span sdktrace.ReadOnlySpan) (*threadify.ThreadInstance, error) {
	traceID := span.SpanContext().TraceID().String()

	e.mu.Lock()
	thread, ok := e.traceThreadMap[traceID]
	e.mu.Unlock()

	if ok {
		return thread, nil
	}

	existingThreadID := attrString(span.Attributes(), "threadify.thread_id")
	if existingThreadID != "" {
		role := attrString(span.Attributes(), "threadify.role")
		if role == "" {
			role = "participant"
		}
		thread, err := e.conn.Join(ctx, threadify.WithJoinThreadID(existingThreadID), threadify.WithJoinRole(role))
		if err != nil {
			return nil, err
		}
		e.cacheThread(traceID, thread)
		return thread, nil
	}

	archived, err := e.conn.GetThreadsByRef(ctx, &threadify.RefQuery{
		RefKey:   "otel_trace_id",
		RefValue: traceID,
	})
	if err == nil && len(archived) > 0 {
		role := attrString(span.Attributes(), "threadify.role")
		if role == "" {
			role = "participant"
		}
		thread, err := e.conn.Join(ctx, threadify.WithJoinThreadID(archived[0].ID), threadify.WithJoinRole(role))
		if err != nil {
			return nil, err
		}
		e.cacheThread(traceID, thread)
		return thread, nil
	}

	contractName := attrString(span.Attributes(), "threadify.contract")
	label := attrString(span.Attributes(), "threadify.label")
	if label == "" {
		label = span.Name()
	}
	// Default to service.name from OTel resource if present
	var resourceServiceName string
	if res := span.Resource(); res != nil {
		for _, attr := range res.Attributes() {
			if string(attr.Key) == "service.name" {
				resourceServiceName = attr.Value.AsString()
				break
			}
		}
	}

	serviceName := attrString(span.Attributes(), "threadify.service")
	if serviceName == "" {
		serviceName = resourceServiceName
	}

	opts := []threadify.StartOption{}
	if serviceName != "" {
		opts = append(opts, threadify.WithService(serviceName))
	}

	// Support both fused.tags and threadify.tags natively
	tags := attrStringSlice(span.Attributes(), "threadify.tags")
	if len(tags) == 0 {
		tags = attrStringSlice(span.Attributes(), "fused.tags")
	}

	// Automatically tag the thread with the service name (e.g. "engine" or "registry")
	if resourceServiceName != "" {
		tags = append(tags, resourceServiceName)
	}

	if len(tags) > 0 {
		opts = append(opts, threadify.WithTags(tags...))
	}

	if contractName != "" {
		opts = append(opts, threadify.WithContract(contractName))
	}

	thread, err = e.conn.Start(ctx, label, opts...)
	if err != nil {
		return nil, err
	}

	e.cacheThread(traceID, thread)
	return thread, nil
}

func (e *ThreadifyExporter) cacheThread(traceID string, thread *threadify.ThreadInstance) {
	e.mu.Lock()
	e.traceThreadMap[traceID] = thread
	e.mu.Unlock()

	time.AfterFunc(10*time.Minute, func() {
		e.mu.Lock()
		delete(e.traceThreadMap, traceID)
		e.mu.Unlock()
	})
}

func refKeySet(refs []string) map[string]bool {
	m := make(map[string]bool, len(refs))
	for _, r := range refs {
		m[r] = true
	}
	return m
}

func attrString(attrs []attribute.KeyValue, key string) string {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

func attrStringSlice(attrs []attribute.KeyValue, key string) []string {
	for _, attr := range attrs {
		if string(attr.Key) != key {
			continue
		}
		switch attr.Value.Type() {
		case attribute.STRINGSLICE:
			return attr.Value.AsStringSlice()
		case attribute.STRING:
			if s := attr.Value.AsString(); s != "" {
				return []string{s}
			}
		}
	}
	return nil
}

func attrValueToAny(v attribute.Value) any {
	switch v.Type() {
	case attribute.BOOL:
		return v.AsBool()
	case attribute.INT64:
		return v.AsInt64()
	case attribute.FLOAT64:
		return v.AsFloat64()
	case attribute.STRING:
		return v.AsString()
	case attribute.BOOLSLICE:
		return v.AsBoolSlice()
	case attribute.INT64SLICE:
		return v.AsInt64Slice()
	case attribute.FLOAT64SLICE:
		return v.AsFloat64Slice()
	case attribute.STRINGSLICE:
		return v.AsStringSlice()
	default:
		return v.AsString()
	}
}

func attrValueToString(v attribute.Value) string {
	switch v.Type() {
	case attribute.STRING:
		return v.AsString()
	default:
		return fmt.Sprintf("%v", attrValueToAny(v))
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func (e *ThreadifyExporter) shouldDrop(name string) bool {
	for _, filter := range e.options.Filters {
		if filter == "" {
			continue
		}
		if strings.HasSuffix(filter, "*") {
			prefix := filter[:len(filter)-1]
			if strings.HasPrefix(name, prefix) {
				return true
			}
			continue
		}
		if name == filter {
			return true
		}
	}
	return false
}

var _ sdktrace.SpanExporter = (*ThreadifyExporter)(nil)
