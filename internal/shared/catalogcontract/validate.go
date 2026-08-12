package catalogcontract

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const MaxSources = 32

var identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

func Validate(composition *Composition) error {
	if composition == nil {
		return nil
	}
	if composition.Version != Version {
		return fmt.Errorf("catalog version must be %d", Version)
	}
	if composition.CollisionPolicy != CollisionReject {
		return errors.New("catalog collision_policy must be reject")
	}
	if len(composition.Sources) < 2 || len(composition.Sources) > MaxSources {
		return fmt.Errorf("catalog sources must contain between 2 and %d entries", MaxSources)
	}
	return validateSources(composition.Sources)
}

func validateSources(sources []Source) error {
	namespaces := make(map[string]struct{}, len(sources))
	prefixes := make(map[string]struct{}, len(sources))
	for index := range sources {
		if err := validateSource(sources[index]); err != nil {
			return fmt.Errorf("sources[%d]: %w", index, err)
		}
		if _, duplicate := namespaces[sources[index].Namespace]; duplicate {
			return fmt.Errorf("sources[%d].namespace is duplicated", index)
		}
		if _, duplicate := prefixes[sources[index].OperationPrefix]; duplicate {
			return fmt.Errorf("sources[%d].operation_prefix is duplicated", index)
		}
		namespaces[sources[index].Namespace] = struct{}{}
		prefixes[sources[index].OperationPrefix] = struct{}{}
	}
	return nil
}

func validateSource(source Source) error {
	if err := validateSourceIdentity(source); err != nil {
		return err
	}
	return validateSourceRef(source.SourceRef)
}

func validateSourceIdentity(source Source) error {
	if !identifierPattern.MatchString(source.Name) || !identifierPattern.MatchString(source.Namespace) || !identifierPattern.MatchString(source.OperationPrefix) {
		return errors.New("name, namespace, and operation_prefix must be bounded identifiers")
	}
	return nil
}

func validateSourceRef(sourceRef string) error {
	if len(sourceRef) > 2048 || strings.ContainsAny(sourceRef, "\r\n\x00") {
		return errors.New("source_ref is invalid")
	}
	parsed, err := url.Parse(sourceRef)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("source_ref is invalid")
	}
	if parsed.Scheme == "uploaded" && parsed.Opaque != "" {
		return nil
	}
	if parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("source_ref must be HTTPS or an uploaded reference")
	}
	return nil
}
