// Package alerttest records alert calls for tests in either module.
//
// Five packages had their own copy of this type — `fakeAlerter`,
// `alertRecorder`, `recordingAlerter` ×2, `alertRec` — and one of the five
// carried a comment arguing against sharing: two copies of a one-field,
// one-method type did not justify a package. That was right at two. At five it
// stopped being right, and the copies had already drifted in the way that
// matters: only the proxy package's copy took a lock, and alerts there arrive
// from goroutines. The other four append to a slice from whatever goroutine
// calls them, which is a data race that only shows up when the code under test
// starts alerting concurrently — a change in the subject, not in the test.
//
// So this one locks. A test recorder that is safe only in the cases that happen
// to be single-threaded today is a trap for whoever makes them not be.
package alerttest

import (
	"context"
	"sync"
)

// Entry is one recorded alert.
type Entry struct{ Subject, Detail string }

// Recorder satisfies every module's Alerter/AlertSink port: they all declare the
// same method, each in its own package (ADR-0156), and Go satisfies them all
// implicitly.
type Recorder struct {
	mu      sync.Mutex
	entries []Entry
}

func (r *Recorder) Alert(_ context.Context, subject, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, Entry{Subject: subject, Detail: detail})
}

// Entries returns a copy of what has been recorded.
func (r *Recorder) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.entries...)
}

// Subjects returns just the subjects, which is what most assertions look at.
func (r *Recorder) Subjects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.Subject
	}
	return out
}

// Count returns how many alerts have been recorded.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}
