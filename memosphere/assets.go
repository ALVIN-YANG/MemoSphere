// Package assets embeds the same browser console for the Go service.
package assets

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var files embed.FS

func Console() fs.FS {
	root, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return root
}
