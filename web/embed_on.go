//go:build webembed

// Admin UI assets, baked into the binary.
//
// `webembed` is one of the two build tags in the tree (the other, `live`, gates
// the live upstream probes in proxy/live_test.go) and it is off by default:
// `//go:embed` fails to compile when the directory is missing, and the built UI
// is a build artifact rather than a checked-in one. Development therefore runs
// without the tag and serves the UI from the Vite dev server; release images
// build the UI first and then compile with `-tags webembed`, so the shipped
// artifact is a single file.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:apps/staff/dist
var staffDist embed.FS

// StaffDist returns the built admin UI.
func StaffDist() fs.FS {
	f, err := fs.Sub(staffDist, "apps/staff/dist")
	if err != nil {
		panic(err)
	}
	return f
}
