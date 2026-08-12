package catalogcontract

import "strings"

func Normalize(composition *Composition) {
	if composition == nil {
		return
	}
	composition.CollisionPolicy = CollisionPolicy(strings.ToLower(strings.TrimSpace(string(composition.CollisionPolicy))))
	for index := range composition.Sources {
		source := &composition.Sources[index]
		source.Name = strings.TrimSpace(source.Name)
		source.Namespace = strings.TrimSpace(source.Namespace)
		source.SourceRef = strings.TrimSpace(source.SourceRef)
		source.OperationPrefix = strings.TrimSpace(source.OperationPrefix)
	}
}

func NormalizeAndValidate(composition *Composition) error {
	Normalize(composition)
	return Validate(composition)
}
