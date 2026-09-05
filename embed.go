// Package autophage: headless variant. No web SPA is embedded; Static returns an
// empty filesystem so the API serves no static UI.
package autophage

import "io/fs"

// Static returns an empty filesystem (no web build in this app).
func Static() fs.FS { return emptyFS{} }

// HasIndex always reports false: there is no embedded index.html.
func HasIndex(fs.FS) bool { return false }

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
