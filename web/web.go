// Package web embeds the console shell, the UI SDK and the shared vendor
// libraries, so the core binary serves its own frontend.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var static embed.FS

// FS returns the console files rooted at static/.
func FS() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
