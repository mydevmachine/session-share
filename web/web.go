package web

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed static templates
var files embed.FS

var Templates = template.Must(template.ParseFS(files, "templates/*.html"))

func Static() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
