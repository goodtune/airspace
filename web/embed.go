// Package web holds the embedded frontend.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var files embed.FS

// Static is the frontend file tree.
func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
