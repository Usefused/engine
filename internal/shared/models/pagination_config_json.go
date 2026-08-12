package models

import (
	"encoding/json"

	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

// UnmarshalJSON delegates to the canonical policy decoder so JSONB reads do
// not bypass the strict Registry-to-Engine contract boundary.
func (p *PaginationConfig) UnmarshalJSON(data []byte) error {
	var decoded paginationpolicy.Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = PaginationConfig(decoded)
	return nil
}
