//go:build !webembed

// Package web is the admin UI's entry point into the binary; this file is the
// no-assets variant (see embed_on.go).
package web

import "io/fs"

// StaffDist returns nil when no assets were embedded. httpx.SPA renders a
// short note pointing at the dev server instead of a 404: "there is no page
// here" and "this build carries no UI" answer very different questions for
// someone wondering whether they configured something wrong.
func StaffDist() fs.FS { return nil }
