// Package web embeds the built frontend into the server binary, so the
// deployed artifact is a single static executable with no runtime file
// dependencies — which is what makes the distroless container possible.
package web

import (
	"embed"
	"io/fs"
)

// The all: prefix includes files Vite may emit with a leading underscore or
// dot, which plain //go:embed silently skips.
//
// dist/index.html is committed as a placeholder so `go build` works before the
// frontend has ever been built; the real Vite build overwrites it.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the built frontend rooted at web/dist, so paths are served as
// "index.html" rather than "dist/index.html".
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive above stops matching, which is
		// a build-time mistake rather than a runtime condition.
		panic("web: embedded dist subtree missing: " + err.Error())
	}
	return sub
}
