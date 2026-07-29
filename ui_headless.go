//go:build headless

package backend

import "net/http"

// GetUIFS returns nil in headless builds so Engine can run without compiling
// any UI assets into the binary.
func GetUIFS() http.FileSystem {
	return nil
}
