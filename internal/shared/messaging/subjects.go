package messaging

import "fmt"

const (
	FusedEngineStream = "FUSED_ENGINE_EVENTS"

	FusedEngineAnalyticsWildcard = "fused_engine.analytics.>"
	FusedEngineSessionWildcard   = "fused_engine.session.>"
	FusedEngineKillWildcard      = "fused_engine.kill.>"

	FusedEngineKillSubscribe    = "fused_engine.kill.*"
	FusedEngineCleanupSubscribe = "fused_engine.cleanup.*"
)

func FusedEngineStreamSubjects() []string {
	return []string{
		FusedEngineAnalyticsWildcard,
		FusedEngineSessionWildcard,
		FusedEngineKillWildcard,
	}
}

func FusedEngineAnalyticsSubject(artifactID string) string {
	return fmt.Sprintf("fused_engine.analytics.%s", artifactID)
}

func FusedEngineSessionSubject(artifactID string) string {
	return fmt.Sprintf("fused_engine.session.%s", artifactID)
}
