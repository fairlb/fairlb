package errcode

import "fmt"

// Register adds a segment of the registry that only some builds ship.
//
// # Why the registry is open at all
//
// The registry is split by layer so that a build compiles in only the codes it
// can actually emit: a code a build has no way of producing is a line in a
// public contract that is not true of that build. But the lookup is not split.
// Everything that renders an error — the problem+json writer, the data plane's
// status table — resolves a bare code string, and it resolves it without
// knowing which layer produced it. Splitting the lookup as well would mean
// every renderer taking a list of registries, and the renderers live below the
// layers whose codes they render.
//
// So the packages are separate and the registry is one. A segment hands its
// definitions over at init time, which ties "this build compiles the codes in"
// to "this build can resolve them": there is no window in which a code exists
// as a constant but resolves to nothing. That window is worth designing away,
// because an unresolved code does not fail loudly — it renders as an internal
// error with the wrong status, which reads like a server fault rather than
// like the missing wiring it is.
//
// # Contract
//
// Call it from a package's init only. The map is read on every error response
// and is not guarded by a lock; init ordering is what makes that safe, since
// Go finishes initialising imported packages before any of them serves
// anything. Registering later races with live traffic.
//
// A code that is already registered panics rather than being overwritten: two
// definitions for one code means one of them is silently not what clients get,
// and picking the winner by init order is not a decision anyone made. The
// generator refuses duplicates across segments too, so reaching this panic
// means a definition arrived from somewhere other than the generator.
func Register(defs map[string]Def) {
	if len(defs) == 0 {
		panic("errcode: Register called with no definitions; an empty segment is a broken generator run, not an empty segment")
	}
	for code, d := range defs {
		if prev, dup := registry[code]; dup {
			panic(fmt.Sprintf("errcode: %s is already registered as %+v; an error code is part of the public contract and cannot have two definitions", code, prev))
		}
		registry[code] = d
	}
}

// Lookup returns a code's definition. Second result false means the code is not
// registered — which for a code that reaches a response is a generator bug, not
// a runtime condition, so callers should fall back to CommonInternal rather than
// inventing a definition.
func Lookup(code string) (Def, bool) {
	d, ok := registry[code]
	return d, ok
}
