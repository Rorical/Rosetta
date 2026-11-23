// Package web holds the console UI. `make build-console` fills dist; the
// tracked placeholder lets Go packages build before that step has run.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Dist returns the built UI rooted at its index.html.
func Dist() fs.FS {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		// Sub only fails on an invalid path, and "dist" is constant.
		return assets
	}
	return dist
}
