package workflowcontract

import (
	"errors"
	"fmt"
	"mime"
	"net/url"
	"regexp"
	"strings"
)

const (
	MaxMediaTypes = 32
	MaxModes      = 3
	MaxSteps      = 4
	MaxPath       = 2048
	MaxUploadSize = int64(1 << 50)
	MaxChunkSize  = int64(1 << 30)
	MaxOrigins    = 16
	MaxStatuses   = 16
)

var (
	methodPattern = regexp.MustCompile(`^[A-Z][A-Z0-9!#$%&'*+.^_` + "`" + `|~-]{0,31}$`)
	headerPattern = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

func Validate(workflow *UploadWorkflow) error {
	if workflow == nil {
		return nil
	}
	if workflow.Version != Version {
		return fmt.Errorf("upload_workflow version must be %d", Version)
	}
	if len(workflow.AcceptedMediaTypes) < 1 || len(workflow.AcceptedMediaTypes) > MaxMediaTypes {
		return fmt.Errorf("accepted_media_types must contain between 1 and %d entries", MaxMediaTypes)
	}
	if workflow.MaxSizeBytes < 0 || workflow.MaxSizeBytes > MaxUploadSize {
		return fmt.Errorf("max_size_bytes must be between 0 and %d", MaxUploadSize)
	}
	if err := validateMediaTypes(workflow.AcceptedMediaTypes); err != nil {
		return err
	}
	return validateModes(workflow.Modes)
}

func validateMediaTypes(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, _, err := mime.ParseMediaType(value); err != nil || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("accepted media type %q is invalid", value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("accepted media type %q is duplicated", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateModes(modes []UploadMode) error {
	if len(modes) < 1 || len(modes) > MaxModes {
		return fmt.Errorf("modes must contain between 1 and %d entries", MaxModes)
	}
	seen := make(map[UploadModeKind]struct{}, len(modes))
	for index := range modes {
		if _, duplicate := seen[modes[index].Kind]; duplicate {
			return fmt.Errorf("modes[%d].kind is duplicated", index)
		}
		seen[modes[index].Kind] = struct{}{}
		if err := validateMode(&modes[index]); err != nil {
			return fmt.Errorf("modes[%d]: %w", index, err)
		}
	}
	return nil
}

func validateMode(mode *UploadMode) error {
	if mode.Kind != UploadSimple && mode.Kind != UploadMultipart && mode.Kind != UploadResumable {
		return fmt.Errorf("kind %q is unsupported", mode.Kind)
	}
	if len(mode.Steps) < 1 || len(mode.Steps) > MaxSteps {
		return fmt.Errorf("steps must contain between 1 and %d entries", MaxSteps)
	}
	for index := range mode.Steps {
		if err := validateStep(mode.Steps[index]); err != nil {
			return fmt.Errorf("steps[%d]: %w", index, err)
		}
	}
	return validateModeSequence(mode)
}

func validateStep(step UploadStep) error {
	if step.Kind != StepInitiate && step.Kind != StepTransfer {
		return fmt.Errorf("kind %q is unsupported", step.Kind)
	}
	if !methodPattern.MatchString(step.Method) {
		return errors.New("method is invalid")
	}
	if step.Body != BodyMetadata && step.Body != BodyMedia && step.Body != BodyMultipart {
		return fmt.Errorf("body %q is unsupported", step.Body)
	}
	if err := validateURLSource(step.URL); err != nil {
		return err
	}
	if err := validateChunking(step.Chunking); err != nil {
		return err
	}
	return validateStepStatuses(step)
}

func validateURLSource(source URLSource) error {
	switch source.Kind {
	case URLDeclaredPath:
		return validateDeclaredPathSource(source)
	case URLResponseHeader:
		return validateResponseHeaderSource(source)
	default:
		return fmt.Errorf("url kind %q is unsupported", source.Kind)
	}
}

func validateDeclaredPathSource(source URLSource) error {
	if source.HeaderName != "" || len(source.AllowedOrigins) != 0 {
		return errors.New("declared_path cannot include header_name or allowed_origins")
	}
	if len(source.Path) > MaxPath || !strings.HasPrefix(source.Path, "/") || strings.ContainsAny(source.Path, "\r\n?#") {
		return errors.New("declared_path requires a bounded absolute path")
	}
	return nil
}

func validateResponseHeaderSource(source URLSource) error {
	if source.Path != "" || !headerPattern.MatchString(source.HeaderName) {
		return errors.New("response_header requires only a valid header_name")
	}
	return validateOrigins(source.AllowedOrigins)
}

func validateOrigins(origins []string) error {
	if len(origins) > MaxOrigins {
		return fmt.Errorf("allowed_origins may contain at most %d entries", MaxOrigins)
	}
	seen := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		if err := validateOrigin(origin); err != nil {
			return err
		}
		if _, duplicate := seen[origin]; duplicate {
			return errors.New("allowed_origin is duplicated")
		}
		seen[origin] = struct{}{}
	}
	return nil
}

func validateOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("allowed_origin must be an origin without path, query, credentials, or fragment")
	}
	if parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil
	}
	return errors.New("allowed_origin must use HTTPS or loopback HTTP")
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func validateStepStatuses(step UploadStep) error {
	if len(step.SuccessStatuses) < 1 || len(step.SuccessStatuses) > MaxStatuses {
		return fmt.Errorf("success_statuses must contain between 1 and %d entries", MaxStatuses)
	}
	if len(step.ContinueStatuses) > MaxStatuses {
		return fmt.Errorf("continue_statuses may contain at most %d entries", MaxStatuses)
	}
	if err := validateStatusRanges(step.SuccessStatuses); err != nil {
		return fmt.Errorf("success_statuses: %w", err)
	}
	if err := validateStatusRanges(step.ContinueStatuses); err != nil {
		return fmt.Errorf("continue_statuses: %w", err)
	}
	if rangesOverlap(step.SuccessStatuses, step.ContinueStatuses) {
		return errors.New("success_statuses and continue_statuses overlap")
	}
	if step.Kind != StepTransfer && len(step.ContinueStatuses) != 0 {
		return errors.New("only transfer steps may declare continue_statuses")
	}
	return nil
}

func validateStatusRanges(ranges []StatusRange) error {
	for index, status := range ranges {
		if status.Min < 100 || status.Max > 599 || status.Min > status.Max {
			return fmt.Errorf("statuses[%d] is invalid", index)
		}
		for previous := 0; previous < index; previous++ {
			if status.Min <= ranges[previous].Max && ranges[previous].Min <= status.Max {
				return fmt.Errorf("statuses[%d] overlaps statuses[%d]", index, previous)
			}
		}
	}
	return nil
}

func rangesOverlap(left, right []StatusRange) bool {
	for _, first := range left {
		for _, second := range right {
			if first.Min <= second.Max && second.Min <= first.Max {
				return true
			}
		}
	}
	return false
}

func validateChunking(chunking *Chunking) error {
	if chunking == nil {
		return nil
	}
	if chunking.DefaultSizeBytes < 1 || chunking.SizeMultipleBytes < 1 || chunking.MaxSizeBytes < chunking.DefaultSizeBytes || chunking.MaxSizeBytes > MaxChunkSize {
		return errors.New("chunking sizes are invalid")
	}
	if chunking.DefaultSizeBytes%chunking.SizeMultipleBytes != 0 || chunking.MaxSizeBytes%chunking.SizeMultipleBytes != 0 {
		return errors.New("chunk sizes must align to size_multiple_bytes")
	}
	return nil
}

func validateModeSequence(mode *UploadMode) error {
	if mode.Kind != UploadResumable {
		return validateDirectModeSequence(mode)
	}
	return validateResumableModeSequence(mode)
}

func validateDirectModeSequence(mode *UploadMode) error {
	if len(mode.Steps) != 1 {
		return errors.New("simple and multipart modes require one step")
	}
	step := mode.Steps[0]
	if step.Kind != StepTransfer || step.URL.Kind != URLDeclaredPath || step.Chunking != nil {
		return errors.New("simple and multipart modes require one declared transfer step without chunking")
	}
	return nil
}

func validateResumableModeSequence(mode *UploadMode) error {
	if len(mode.Steps) != 2 {
		return errors.New("resumable mode requires exactly two steps")
	}
	if mode.Steps[0].Kind != StepInitiate || mode.Steps[0].URL.Kind != URLDeclaredPath {
		return errors.New("resumable mode must begin with one declared initiation step")
	}
	if mode.Steps[1].Kind != StepTransfer || mode.Steps[1].URL.Kind != URLResponseHeader || mode.Steps[1].Chunking == nil {
		return errors.New("resumable mode must end with one response-header transfer step with chunking")
	}
	return nil
}
