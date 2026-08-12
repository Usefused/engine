// Package catalogcontract owns deterministic composition of separately
// published provider specifications without merging component namespaces.
package catalogcontract

type CollisionPolicy string

const (
	Version                         = 1
	CollisionReject CollisionPolicy = "reject"
)

type Composition struct {
	Version         int             `json:"version" yaml:"version"`
	CollisionPolicy CollisionPolicy `json:"collision_policy" yaml:"collision_policy"`
	Sources         []Source        `json:"sources" yaml:"sources"`
}

type Source struct {
	Name            string `json:"name" yaml:"name"`
	Namespace       string `json:"namespace" yaml:"namespace"`
	SourceRef       string `json:"source_ref" yaml:"source_ref"`
	OperationPrefix string `json:"operation_prefix" yaml:"operation_prefix"`
}
