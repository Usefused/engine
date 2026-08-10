package messaging

import "fmt"

const (
	FusedEngineStream = "FUSED_ENGINE_EVENTS"
	// ProviderRateLimitBucket is a separate compacted JetStream stream. Its
	// latest value is the live quota authority; PostgreSQL consumes revisions
	// only as an eventual projection.
	ProviderRateLimitBucket = "FUSED_PROVIDER_RATE_LIMITS"

	FusedEngineSessionWildcard   = "fused_engine.session.>"
	FusedEngineKillWildcard      = "fused_engine.kill.>"
	EngineExecutionEventsSubject = "engine.execution.events.v1"

	FusedEngineKillSubscribe    = "fused_engine.kill.*"
	FusedEngineCleanupSubscribe = "fused_engine.cleanup.*"
)

func ProviderRateLimitKVStream() string {
	return "KV_" + ProviderRateLimitBucket
}

func ProviderRateLimitKVSubject() string {
	return "$KV." + ProviderRateLimitBucket + ".>"
}

func FusedEngineStreamSubjects() []string {
	return []string{
		FusedEngineSessionWildcard,
		FusedEngineKillWildcard,
		EngineExecutionEventsSubject,
	}
}

func FusedEngineSessionSubject(appID string) string {
	return fmt.Sprintf("fused_engine.session.%s", appID)
}
