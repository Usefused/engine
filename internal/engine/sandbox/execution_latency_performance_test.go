package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	latencyEvidenceSamples = 40
	latencyEvidenceSecret  = "latency-evidence-must-not-leak"
)

type latencyEvidence struct {
	engine   []time.Duration
	provider []time.Duration
	overhead []time.Duration
}

func TestExecutionLatencySeparatesEngineAndProviderPercentiles(t *testing.T) {
	server := latencyEvidenceProvider(t)
	recorder, provider := latencyEvidenceTracer(t)
	installLatencyEvidenceExecution(t, server.URL)

	evidence := latencyEvidence{
		engine: make([]time.Duration, 0, latencyEvidenceSamples), provider: make([]time.Duration, 0, latencyEvidenceSamples),
		overhead: make([]time.Duration, 0, latencyEvidenceSamples),
	}
	for sample := 0; sample < latencyEvidenceSamples; sample++ {
		total, providerTime := executeLatencyEvidenceSample(t, recorder, provider, sample)
		evidence.engine = append(evidence.engine, total)
		evidence.provider = append(evidence.provider, providerTime)
		evidence.overhead = append(evidence.overhead, total-providerTime)
	}

	assertLatencyEvidence(t, evidence)
	assertLatencySpansAreSecretSafe(t, recorder.Ended(), latencyEvidenceSecret, server.URL, strings.TrimPrefix(server.URL, "http://"))
	logLatencyEvidence(t, evidence)
}

func latencyEvidenceProvider(t *testing.T) *httptest.Server {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("evidence_token") != latencyEvidenceSecret {
			http.Error(response, "missing evidence token", http.StatusUnauthorized)
			return
		}
		// A repeating delay distribution makes provider percentiles meaningful
		// without relying on a remote provider or an uncontrolled network.
		delay := time.Duration(2+calls.Add(1)%4) * time.Millisecond
		time.Sleep(delay)
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func latencyEvidenceTracer(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.Background())
	})
	return recorder, provider
}

func installLatencyEvidenceExecution(t *testing.T, providerURL string) {
	t.Helper()
	previous := EngineStreamExecuteFunc
	dispatcher := engine.NewDispatcher()
	service := &models.Service{Name: "latency-evidence", BaseURL: providerURL}
	endpoint := &models.IntegrationObject{
		Name: "latencyEvidence", Method: http.MethodGet, Path: "/items",
		Parameters:           models.Parameters{{Name: "evidence_token", In: "query", Type: "string"}},
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}},
	}
	EngineStreamExecuteFunc = func(
		ctx context.Context, _, _, _ string, params map[string]any, _ map[string]any, _ string, stream engine.ResponseStream,
	) error {
		_, err := dispatcher.ExecuteStream(ctx, service, endpoint, params, nil, nil, stream)
		return err
	}
	t.Cleanup(func() { EngineStreamExecuteFunc = previous })
}

func executeLatencyEvidenceSample(t *testing.T, recorder *tracetest.SpanRecorder, provider *sdktrace.TracerProvider, sample int) (time.Duration, time.Duration) {
	t.Helper()
	spanName := fmt.Sprintf("engine.latency.evidence.%02d", sample)
	ctx, span := provider.Tracer("release-evidence").Start(context.Background(), spanName)
	stream := &fakeExecuteStream{ctx: ctx}
	request := &enginev1.ExecuteRequest{
		AppId: "latency-evidence", EndpointName: "latencyEvidence",
		Params: []byte(fmt.Sprintf(`{"evidence_token":%q}`, latencyEvidenceSecret)),
	}
	if err := NewEngineGRPCServer().Execute(request, stream); err != nil {
		span.End()
		t.Fatalf("execute latency evidence sample: %v", err)
	}
	span.End()
	recorded := recordedLatencySpan(t, recorder.Ended(), spanName)
	return recordedTiming(t, recorded, "engine_total"), recordedTiming(t, recorded, "provider_total")
}

func recordedLatencySpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q was not recorded", name)
	return nil
}

func recordedTiming(t *testing.T, span sdktrace.ReadOnlySpan, name string) time.Duration {
	t.Helper()
	key := "engine.timing." + name + "_ms"
	for _, attribute := range span.Attributes() {
		if string(attribute.Key) == key {
			return time.Duration(attribute.Value.AsFloat64() * float64(time.Millisecond))
		}
	}
	t.Fatalf("span %q is missing %q", span.Name(), key)
	return 0
}

func assertLatencyEvidence(t *testing.T, evidence latencyEvidence) {
	t.Helper()
	for index := range evidence.engine {
		if evidence.provider[index] <= 0 {
			t.Fatalf("provider sample %d was not recorded: %s", index, evidence.provider[index])
		}
		if evidence.overhead[index] < 0 {
			t.Fatalf("Engine total was shorter than provider time at sample %d: total=%s provider=%s", index, evidence.engine[index], evidence.provider[index])
		}
	}
	for _, samples := range [][]time.Duration{evidence.engine, evidence.provider, evidence.overhead} {
		if percentileDuration(samples, 50) > percentileDuration(samples, 95) || percentileDuration(samples, 95) > percentileDuration(samples, 99) {
			t.Fatalf("latency percentiles are not monotonic: %v", samples)
		}
	}
}

func percentileDuration(samples []time.Duration, percentile int) time.Duration {
	ordered := append([]time.Duration(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := (len(ordered)*percentile + 99) / 100
	if index > 0 {
		index--
	}
	return ordered[index]
}

func assertLatencySpansAreSecretSafe(t *testing.T, spans []sdktrace.ReadOnlySpan, forbidden ...string) {
	t.Helper()
	for _, span := range spans {
		for _, attribute := range span.Attributes() {
			serialized := fmt.Sprint(attribute.Value.AsInterface())
			for _, value := range forbidden {
				if strings.Contains(serialized, value) {
					t.Fatalf("span %q attribute %q leaked %q", span.Name(), attribute.Key, value)
				}
			}
		}
	}
}

func logLatencyEvidence(t *testing.T, evidence latencyEvidence) {
	t.Helper()
	t.Logf(
		"Engine latency samples=%d total[p50=%s p95=%s p99=%s] provider[p50=%s p95=%s p99=%s] overhead[p50=%s p95=%s p99=%s]",
		len(evidence.engine),
		percentileDuration(evidence.engine, 50), percentileDuration(evidence.engine, 95), percentileDuration(evidence.engine, 99),
		percentileDuration(evidence.provider, 50), percentileDuration(evidence.provider, 95), percentileDuration(evidence.provider, 99),
		percentileDuration(evidence.overhead, 50), percentileDuration(evidence.overhead, 95), percentileDuration(evidence.overhead, 99),
	)
}
