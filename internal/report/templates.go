package report

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed templates/*.hbs
var templateFS embed.FS

// Templates returns the embedded PVE notification templates, keyed by file
// name (e.g. "pbp-body.txt.hbs"). Each call returns fresh copies.
func Templates() map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(templateFS, "templates")
	if err != nil {
		panic("report: embedded templates: " + err.Error())
	}
	for _, e := range entries {
		b, err := templateFS.ReadFile(path.Join("templates", e.Name()))
		if err != nil {
			panic("report: embedded template " + e.Name() + ": " + err.Error())
		}
		out[e.Name()] = b
	}
	return out
}
