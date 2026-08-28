package video

import "slices"

// Vendors lists every vendor this build can serve video for, sorted.
//
// It lives in a _test.go file because nothing in production enumerates the
// registry -- the data plane resolves one vendor at a time through MapperFor.
// The two suites that walk every registered vendor are in this package, so a
// test-only export is all this ever was.
func Vendors() []string {
	out := make([]string, 0, len(registry))
	for v := range registry {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}
