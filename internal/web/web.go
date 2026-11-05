// Package web embeds the web UI: plain ES modules, CSS and one HTML page,
// with no build step. Nothing loads from a CDN, so the UI works offline.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// FS is the static file tree served at /.
var FS fs.FS

func init() {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(err)
	}
	FS = sub
}
