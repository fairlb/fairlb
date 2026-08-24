package settings

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sync"
)

// Resolver turns a section's settings into a built client — a payment
// channel, a mail sender, an OAuth provider — at call time rather than at
// boot.
//
// The clients used to be constructed once in the entrypoint from ENV, which is
// why changing a credential meant a deploy. Resolving on each call would
// reconstruct them on every request; this keeps one built value and rebuilds
// only when the underlying settings change. The change detection is a
// fingerprint over the effective values, read through the Store's cache, so a
// call costs a handful of cache hits and, across instances, the invalidation
// broadcast is what makes a write visible everywhere within the cache TTL.
//
// A build that fails is remembered with its fingerprint: the same broken
// configuration does not get rebuilt — and re-logged — on every call, and the
// next successful write clears it.
type Resolver[T any] struct {
	store    *Store
	sections []string
	build    func(Values) (T, error)

	mu  sync.Mutex
	fp  [sha256.Size]byte
	has bool
	cur T
	err error
}

// NewResolver builds a Resolver over the given sections. build receives the
// effective values of every key in them (defaults overlaid with stored values,
// secrets opened) and returns the client, or an error when the values do not
// describe one.
func NewResolver[T any](store *Store, build func(Values) (T, error), sections ...string) *Resolver[T] {
	return &Resolver[T]{store: store, sections: sections, build: build}
}

// Get returns the client for the current settings.
//
// The read, the fingerprint comparison and the store all happen under one
// lock. Reading outside it looked cheaper, but two calls straddling a write
// could then commit in the wrong order: the one that read the old values last
// would store the old client under the old fingerprint, and because that
// fingerprint then matched what it believed current, the stale client served
// until the *next* settings change. The read is a handful of cache hits, so
// serialising it costs nothing measurable.
func (r *Resolver[T]) Get(ctx context.Context) (T, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values, err := r.values(ctx)
	if err != nil {
		// A read failure — a database blip — keeps serving the last built
		// value when there is one: the settings did not change, only the
		// ability to check whether they did. With nothing built yet the error
		// is the answer.
		if r.has && r.err == nil {
			return r.cur, nil
		}
		var zero T
		return zero, err
	}
	fp := fingerprint(values)
	if r.has && fp == r.fp {
		return r.cur, r.err
	}
	r.cur, r.err = r.build(values)
	r.fp, r.has = fp, true
	return r.cur, r.err
}

// values reads the sections' effective values through the cache. An
// unreadable secret is treated as unset, the same reading loadValues gives a
// write: the channel wants its credentials again, nothing else is wrong.
func (r *Resolver[T]) values(ctx context.Context) (Values, error) {
	values := Values{}
	for _, section := range r.sections {
		for _, spec := range r.store.reg.SectionKeys(section) {
			if len(spec.Default) > 0 && spec.Kind != KindSecret {
				values[spec.Key] = spec.Default
			}
			raw, found, err := r.store.value(ctx, spec.Key)
			if errors.Is(err, ErrSecretUnreadable) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if found {
				values[spec.Key] = raw
			}
		}
	}
	return values, nil
}

func fingerprint(values Values) [sha256.Size]byte {
	// json.Marshal of a map writes keys in sorted order, so equal values give
	// equal bytes regardless of the order they were read in.
	b, _ := json.Marshal(values)
	return sha256.Sum256(b)
}
