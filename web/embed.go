// Package web embeds the static PWA.
package web

import (
	"embed"
	"io/fs"
)

//go:embed public
var files embed.FS

func Public() fs.FS {
	s, _ := fs.Sub(files, "public")
	return s
}
