package httpx

import "testing"

func TestParseAddrAcceptsBareAndPortedForms(t *testing.T) {
	for _, in := range []string{"203.0.113.9", "203.0.113.9:4431", "[2001:db8::1]:443", "2001:db8::1"} {
		if got := ParseAddr(in); got == nil {
			t.Errorf("%q should parse", in)
		}
	}
	for _, in := range []string{"", "not-an-ip", "203.0.113.9:port"} {
		if got := ParseAddr(in); got != nil {
			t.Errorf("%q should not parse, got %v", in, got)
		}
	}
}
