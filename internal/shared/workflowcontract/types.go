// Package workflowcontract owns explicit media-upload workflows. Ordinary
// request bodies stay in the HTTP contract; this package models only the
// multi-request protocol needed for resumable or alternate media uploads.
package workflowcontract

type UploadModeKind string
type UploadStepKind string
type UploadBodyKind string
type URLSourceKind string

const (
	Version = 1

	UploadSimple    UploadModeKind = "simple"
	UploadMultipart UploadModeKind = "multipart"
	UploadResumable UploadModeKind = "resumable"

	StepInitiate UploadStepKind = "initiate"
	StepTransfer UploadStepKind = "transfer"

	BodyMetadata  UploadBodyKind = "metadata"
	BodyMedia     UploadBodyKind = "media"
	BodyMultipart UploadBodyKind = "multipart"

	URLDeclaredPath   URLSourceKind = "declared_path"
	URLResponseHeader URLSourceKind = "response_header"
)

type UploadWorkflow struct {
	Version            int          `json:"version" yaml:"version"`
	AcceptedMediaTypes []string     `json:"accepted_media_types" yaml:"accepted_media_types"`
	MaxSizeBytes       int64        `json:"max_size_bytes,omitempty" yaml:"max_size_bytes,omitempty"`
	Modes              []UploadMode `json:"modes" yaml:"modes"`
}

type UploadMode struct {
	Kind  UploadModeKind `json:"kind" yaml:"kind"`
	Steps []UploadStep   `json:"steps" yaml:"steps"`
}

// Steps are ordered. A resumable transfer may consume only a URL returned by
// its immediately preceding initiation step, preventing arbitrary URL input.
type UploadStep struct {
	Kind             UploadStepKind `json:"kind" yaml:"kind"`
	Method           string         `json:"method" yaml:"method"`
	URL              URLSource      `json:"url" yaml:"url"`
	Body             UploadBodyKind `json:"body" yaml:"body"`
	Chunking         *Chunking      `json:"chunking,omitempty" yaml:"chunking,omitempty"`
	SuccessStatuses  []StatusRange  `json:"success_statuses" yaml:"success_statuses"`
	ContinueStatuses []StatusRange  `json:"continue_statuses" yaml:"continue_statuses"`
}

type URLSource struct {
	Kind       URLSourceKind `json:"kind" yaml:"kind"`
	Path       string        `json:"path,omitempty" yaml:"path,omitempty"`
	HeaderName string        `json:"header_name,omitempty" yaml:"header_name,omitempty"`
	// Omission means same-origin only. A response URL may cross origin only
	// when its normalized origin appears here; redirects cannot expand it.
	AllowedOrigins []string `json:"allowed_origins,omitempty" yaml:"allowed_origins,omitempty"`
}

type StatusRange struct {
	Min int `json:"min" yaml:"min"`
	Max int `json:"max" yaml:"max"`
}

type Chunking struct {
	DefaultSizeBytes  int64 `json:"default_size_bytes" yaml:"default_size_bytes"`
	SizeMultipleBytes int64 `json:"size_multiple_bytes" yaml:"size_multiple_bytes"`
	MaxSizeBytes      int64 `json:"max_size_bytes" yaml:"max_size_bytes"`
}
