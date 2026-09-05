// Package app is the module-root package; it embeds the built web SPA so appd
// can serve it from a single binary. The embed lives at the module root (not
// under web/) so the --no-web variant can delete web/ and replace only this
// file. The package is named `app` (a token init.sh rewrites to <name>), and
// callers import it under the stable alias `rootapp`.
package app

import (
	"embed"
	"io/fs"
)

//go:embed all:web/dist
var dist embed.FS

// Static returns the built SPA as a filesystem rooted at web/dist. When the
// web layer has not been built (only .gitkeep present), HasIndex reports
// false and callers should skip mounting it.
func Static() fs.FS {
	sub, err := fs.Sub(dist, "web/dist")
	if err != nil {
		// web/dist is embedded above, so Sub cannot fail; panic guards a
		// future refactor that breaks the path.
		panic(err)
	}
	return sub
}

// HasIndex reports whether fsys contains an index.html (i.e. a real build,
// not just the .gitkeep placeholder).
func HasIndex(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}
