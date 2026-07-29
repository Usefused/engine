//go:build !headless

package backend

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:ui-build
var embeddedUI embed.FS

// GetUIFS returns the embedded UI as an http.FileSystem.
// It strips the "ui-build" prefix so the root of the FS is the root of the web app.
func GetUIFS() http.FileSystem {
	fsys, err := fs.Sub(embeddedUI, "ui-build")
	if err != nil {
		panic(err)
	}
	return http.FS(fsys)
}
