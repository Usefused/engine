package api

import (
	"errors"
	"strings"

	"github.com/Usefused/engine/internal/shared/managedpublication"
)

// validateManagedApplicationReference forbids ignored selectors from quietly choosing a different credential source.
func validateManagedApplicationReference(ref, id string) error {
	_, err := managedpublication.Selector([]string{id})
	// A named application must always accompany the reserved broker reference.
	if err != nil || (id != "" && !strings.HasPrefix(ref, "${fused.bucket.auth.")) {
		return errors.New("managed_application_id requires a managed ref and a canonical nonzero UUID")
	}
	return nil
}
