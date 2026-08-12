package workflowcontract

import "strings"

func Normalize(workflow *UploadWorkflow) {
	if workflow == nil {
		return
	}
	if workflow.AcceptedMediaTypes == nil {
		workflow.AcceptedMediaTypes = []string{}
	}
	for index := range workflow.AcceptedMediaTypes {
		workflow.AcceptedMediaTypes[index] = strings.ToLower(strings.TrimSpace(workflow.AcceptedMediaTypes[index]))
	}
	for modeIndex := range workflow.Modes {
		mode := &workflow.Modes[modeIndex]
		mode.Kind = UploadModeKind(strings.ToLower(strings.TrimSpace(string(mode.Kind))))
		for stepIndex := range mode.Steps {
			normalizeStep(&mode.Steps[stepIndex])
		}
	}
}

func NormalizeAndValidate(workflow *UploadWorkflow) error {
	Normalize(workflow)
	return Validate(workflow)
}

func normalizeStep(step *UploadStep) {
	step.Kind = UploadStepKind(strings.ToLower(strings.TrimSpace(string(step.Kind))))
	step.Method = strings.ToUpper(strings.TrimSpace(step.Method))
	step.Body = UploadBodyKind(strings.ToLower(strings.TrimSpace(string(step.Body))))
	step.URL.Kind = URLSourceKind(strings.ToLower(strings.TrimSpace(string(step.URL.Kind))))
	step.URL.Path = strings.TrimSpace(step.URL.Path)
	step.URL.HeaderName = strings.TrimSpace(step.URL.HeaderName)
	if step.URL.AllowedOrigins == nil {
		step.URL.AllowedOrigins = []string{}
	}
	if step.SuccessStatuses == nil {
		step.SuccessStatuses = []StatusRange{}
	}
	if step.ContinueStatuses == nil {
		step.ContinueStatuses = []StatusRange{}
	}
}
