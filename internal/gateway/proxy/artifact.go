package proxy

import (
	"context"
	"errors"
	"io"
)

// Artifacts is where a finished job's bytes live.
//
// # Why this is a port
//
// Object storage is a whole subsystem and not every deployment has one, which
// is the same reason settle.Settler is a port. The gateway declares what it
// needs; the assembly point decides who provides it. Cloud binds an
// object-store implementation; Community binds NoCustody and proxies the bytes
// from the upstream on read, so PostgreSQL stays its only required dependency
// (ADR-0222).
//
// The two behave identically from outside -- GET /v1/videos/{id}/content
// returns the video either way -- and differ only in how long that keeps
// working. That difference is documented rather than hidden: a deployment that
// takes no custody can promise "while the upstream still has it", and only one
// that does can promise a retention window.
//
// No generated database type crosses this seam, for the reason settle spells
// out: letting one implementation's data model cross welds it into everything
// on this side.
type Artifacts interface {
	// Takes reports whether this store takes custody. The no-custody binding
	// answers false, which is what tells the reconciler not to spend a download
	// it would only throw away.
	Takes() bool

	// Put takes custody and returns the key it chose, so the caller records
	// what the store decided rather than assuming its layout.
	Put(ctx context.Context, ref ArtifactRef, r io.Reader, size int64) (key string, err error)

	// Open streams an artifact back. ErrArtifactGone means it is not there --
	// expired, swept, or never stored -- which the handler renders as 410
	// rather than 500: an artifact past its retention is a normal outcome, not
	// a fault.
	Open(ctx context.Context, key string) (io.ReadCloser, ArtifactInfo, error)

	// Delete releases custody. Idempotent, like settle.Settler.Void, because
	// the retention sweep must be able to run twice over the same row.
	Delete(ctx context.Context, key string) error
}

// ArtifactRef identifies one job's output.
type ArtifactRef struct {
	OrgID       string
	JobID       string
	Index       int
	ContentType string
}

// ArtifactInfo is what the handler needs to write the response headers.
type ArtifactInfo struct {
	SizeBytes   int64
	ContentType string
}

// ErrArtifactGone means the bytes are no longer available here.
var ErrArtifactGone = errors.New("proxy: the artifact is no longer available")

// NoCustody is the binding for a deployment with no object store.
//
// It refuses to Put rather than silently discarding: a store that pretends to
// take custody is worse than one that plainly does not, because the difference
// only surfaces when a caller comes back for the bytes and they are gone with
// no record of why.
type NoCustody struct{}

func (NoCustody) Takes() bool { return false }

func (NoCustody) Put(context.Context, ArtifactRef, io.Reader, int64) (string, error) {
	return "", errors.New("proxy: this deployment does not store video artifacts")
}

func (NoCustody) Open(context.Context, string) (io.ReadCloser, ArtifactInfo, error) {
	return nil, ArtifactInfo{}, ErrArtifactGone
}

func (NoCustody) Delete(context.Context, string) error { return nil }
