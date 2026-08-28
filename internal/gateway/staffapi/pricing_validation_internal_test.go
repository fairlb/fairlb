package gwstaffapi

import "testing"

// Cloudflare re-compresses responses at the edge and rewrites the strong ETag
// this API issued into `W/"v"`; the browser then echoes that weak tag in
// If-Match. The validator strips the weakening rather than rejecting it, so a
// staff console behind the tunnel can still write (every save 400-ed before).
func TestValidateIfMatchAcceptsEdgeWeakenedETags(t *testing.T) {
	accepted := []struct{ in, want string }{
		{`"abc"`, `"abc"`},
		{`W/"abc"`, `"abc"`},
		{` W/"abc" `, `"abc"`},
	}
	for _, c := range accepted {
		v := c.in
		if err := validateIfMatch(&v); err != nil {
			t.Errorf("validateIfMatch(%q) = %v, want nil", c.in, err)
		} else if v != c.want {
			t.Errorf("validateIfMatch(%q) normalized to %q, want %q", c.in, v, c.want)
		}
	}
	// Weakening is the only tolerated deviation: everything else that is not a
	// strong ETag still fails, including a lowercase prefix (RFC 9110 spells
	// the weak marker `W/` exactly) and a weak marker with nothing behind it.
	rejected := []string{``, `""`, `abc`, `W/`, `W/abc`, `w/"abc"`, `"a"b"`}
	for _, in := range rejected {
		v := in
		if err := validateIfMatch(&v); err == nil {
			t.Errorf("validateIfMatch(%q) = nil, want error", in)
		}
	}
}
