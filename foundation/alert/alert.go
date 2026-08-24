// Package alert is the one declaration of "tell an operator that something
// needs a person".
//
// Eight packages declared this same two-string port independently — the proxy
// pipeline, usage probing, the gateway module, the staff API, and four Cloud
// workers. Eight identical declarations are not eight decisions; they are one
// decision written eight times, and the copies drift: a ninth caller reads
// whichever one is nearest and can just as easily invent a third parameter.
//
// It lives in foundation rather than in gateway because of the direction of the
// existing imports: `gateway` imports `proxy` and `usage`, so a port declared in
// `gateway` is unreachable from the two packages that raise the most alerts.
// A port belongs upstream of everyone who speaks through it.
package alert

import "context"

// Sink receives an operator-facing alert.
//
// Two strings, and deliberately no error return. The caller is always on a path
// where the real work has already succeeded or already failed on its own terms —
// a settlement recorded, a backup finished, a probe judged — and what happens to
// the notification must not change that verdict. An implementation that cannot
// deliver logs and moves on; there is nothing useful for the caller to do with
// a delivery failure, and giving it one invites exactly the wrong handling.
type Sink interface {
	Alert(ctx context.Context, subject, detail string)
}

// SinkFunc adapts a plain function to Sink.
type SinkFunc func(ctx context.Context, subject, detail string)

func (f SinkFunc) Alert(ctx context.Context, subject, detail string) { f(ctx, subject, detail) }

// Discard drops every alert. It exists for tests and for builds that genuinely
// have nowhere to send one — never as a default in production assembly, where a
// silent sink means an operator is not being told something the code decided
// they should be told.
var Discard Sink = SinkFunc(func(context.Context, string, string) {})
