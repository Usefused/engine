package messaging

import "fmt"

const (
	FusedEngineStream = "FUSED_ENGINE_EVENTS"

	FusedEngineSessionWildcard   = "fused_engine.session.>"
	FusedEngineKillWildcard      = "fused_engine.kill.>"
	EngineExecutionEventsSubject = "engine.execution.events.v1"

	FusedEngineKillSubscribe    = "fused_engine.kill.*"
	FusedEngineCleanupSubscribe = "fused_engine.cleanup.*"
)

func FusedEngineStreamSubjects() []string {
	return []string{
		FusedEngineSessionWildcard,
		FusedEngineKillWildcard,
		EngineExecutionEventsSubject,
	}
}

func FusedEngineSessionSubject(artifactID string) string {
	return fmt.Sprintf("fused_engine.session.%s", artifactID)
}
