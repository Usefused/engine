package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const maxExecutionCapabilityCountAttribute = 1000

// recordExecutionContractNegotiation emits only bounded counts and stable
// reasons so an untrusted capability name never enters telemetry.
func recordExecutionContractNegotiation(span trace.Span, envelope fusedobject.ExecutionContractEnvelope, err error) {
	outcome := "accepted"
	reason := "supported"
	version := envelope.ContractVersion
	capabilityCount := len(envelope.RequiredCapabilities)
	if details, incompatible := fusedobject.ExecutionContractCompatibilityDetails(err); incompatible {
		outcome = "rejected"
		reason = details.Reason
		version = details.ContractVersion
		capabilityCount = details.CapabilityCount
	}
	span.SetAttributes(
		attribute.Int("execution.contract_version", version),
		attribute.Int("execution.required_capabilities_count", boundedExecutionCapabilityCount(capabilityCount)),
		attribute.String("execution.contract_negotiation.outcome", outcome),
		attribute.String("execution.contract_negotiation.reason", reason),
	)
	if outcome == "rejected" {
		span.SetAttributes(attribute.String("execution.failure_code", fusedobject.ExecutionCapabilityRequiredCode))
		span.SetStatus(codes.Error, fusedobject.ExecutionCapabilityRequiredCode)
	}
}

func boundedExecutionCapabilityCount(count int) int {
	if count < 0 {
		return 0
	}
	if count > maxExecutionCapabilityCountAttribute {
		return maxExecutionCapabilityCountAttribute
	}
	return count
}
