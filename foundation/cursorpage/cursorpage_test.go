package cursorpage

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 1, 12, 30, 45, 123456000, time.UTC)
	id := "0198c5f2-1111-7abc-9def-0123456789ab"
	ts2, id2, err := Decode(Encode(ts, id))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !ts2.Equal(ts) || id2 != id {
		t.Fatalf("the round trip changed the value: %v %q", ts2, id2)
	}
}

func TestDecodeInvalid(t *testing.T) {
	// Every malformed shape must be ErrInvalid, which is what makes the
	// rendering layer answer 400 rather than 500.
	cases := map[string]string{
		"bad base64":       "%%%not-base64%%%",
		"no separator":     enc("1690000000000000"),
		"empty id":         enc("1690000000000000:"),
		"non-numeric time": enc("abc:0198c5f2-1111-7abc-9def-0123456789ab"),
	}
	for name, in := range cases {
		if _, _, err := Decode(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func enc(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func TestClamp(t *testing.T) {
	cases := []struct{ in, def, max, want int }{
		{0, 20, 100, 20},
		{-5, 20, 100, 20},
		{50, 20, 100, 50},
		{500, 20, 100, 100},
		{1, 20, 100, 1},
	}
	for _, c := range cases {
		if got := Clamp(c.in, c.def, c.max); got != c.want {
			t.Errorf("Clamp(%d,%d,%d)=%d want %d", c.in, c.def, c.max, got, c.want)
		}
	}
}

func TestTrim(t *testing.T) {
	rows := []int{1, 2, 3}
	if page, more := Trim(rows, 3); more || len(page) != 3 {
		t.Error("exactly limit rows must not report another page; that is the len == limit mistake")
	}
	if page, more := Trim(rows, 2); !more || len(page) != 2 {
		t.Error("the limit+1 probe should trim back to 2 rows and report more")
	}
	if page, more := Trim([]int{}, 2); more || len(page) != 0 {
		t.Error("empty input")
	}
}

func TestParse(t *testing.T) {
	at := time.Date(2026, 8, 19, 3, 4, 5, 0, time.UTC)
	cur := Encode(at, "0198c5f2-1111-7abc-9def-0123456789ab")
	limit := 500
	page, err := Parse(&cur, &limit, 50, 200)
	if err != nil {
		t.Fatal(err)
	}
	if page.Limit != 200 || page.ProbeLimit() != 201 || !page.CursorAt.Time.Equal(at) || !page.CursorID.Valid {
		t.Fatalf("unexpected parsed page: %+v", page)
	}
	bad := "not-a-cursor"
	if _, err := Parse(&bad, nil, 50, 200); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid cursor should return ErrInvalid, got %v", err)
	}
}

// The text-keyed cursor: what it has to survive, and what it has to refuse.
func TestKeyCursorRoundTripsAndRefuses(t *testing.T) {
	// A separator that would break any printable choice. Credential names are
	// free text and this is the shape that eats a ':' or a '|' scheme.
	parts := []string{"azure", "eu:prod|west,1"}
	got, err := DecodeKey(EncodeKey(parts...), 2)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(got) != 2 || got[0] != parts[0] || got[1] != parts[1] {
		t.Fatalf("round trip = %q, want %q", got, parts)
	}

	// An empty component is a legitimate value (base_url is nullable, a name
	// could in principle be empty), and it must survive rather than collapse
	// the arity.
	if got, err = DecodeKey(EncodeKey("", "x"), 2); err != nil || got[0] != "" {
		t.Fatalf("empty leading component: %q %v", got, err)
	}

	// Wrong arity is refused rather than truncated. A cursor minted by another
	// endpoint would otherwise seek into the wrong order and return a page that
	// looks plausible and is wrong.
	if _, err = DecodeKey(EncodeKey("a", "b"), 3); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arity mismatch = %v, want ErrInvalid", err)
	}
	if _, err = DecodeKey(EncodeKey("a", "b"), 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("arity mismatch (fewer) = %v, want ErrInvalid", err)
	}
	// Not base64 at all.
	if _, err = DecodeKey("!!!not base64!!!", 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed base64 = %v, want ErrInvalid", err)
	}
}

func TestParseKeyPageClampsAndRefuses(t *testing.T) {
	page, err := ParseKeyPage(nil, nil, 2, 20, 100)
	if err != nil || page.Limit != 20 || page.HasKey() {
		t.Fatalf("absent cursor: %+v %v", page, err)
	}
	// At() past the end is "" so a first-page query still has a value for every
	// positional parameter.
	if page.At(0) != "" || page.At(5) != "" {
		t.Fatalf("At past the end should be empty: %q %q", page.At(0), page.At(5))
	}
	over := 1000
	if page, err = ParseKeyPage(nil, &over, 2, 20, 100); err != nil || page.Limit != 100 {
		t.Fatalf("clamp: %+v %v", page, err)
	}
	cur := EncodeKey("azure", "eu")
	if page, err = ParseKeyPage(&cur, nil, 2, 20, 100); err != nil || page.At(1) != "eu" {
		t.Fatalf("continuation: %+v %v", page, err)
	}
	if !page.HasKey() {
		t.Fatal("a continuation must report HasKey")
	}
	if _, err = ParseKeyPage(&cur, nil, 3, 20, 100); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong arity through ParseKeyPage = %v, want ErrInvalid", err)
	}
}

func TestKeyPageBoolAtRejectsForeignComponents(t *testing.T) {
	if v, err := (KeyPage{}).BoolAt(0); err != nil || v {
		t.Fatalf("first page reads false: %v %v", v, err)
	}
	for in, want := range map[string]bool{"t": true, "f": false} {
		if v, err := (KeyPage{Key: []string{in, "slug"}}).BoolAt(0); err != nil || v != want {
			t.Fatalf("%q: %v %v", in, v, err)
		}
	}
	// A two-component cursor from another endpoint (vendor, slug) has the
	// right arity and the wrong domain: it must not read as "not default".
	if _, err := (KeyPage{Key: []string{"openai", "slug"}}).BoolAt(0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign component should be ErrInvalid: %v", err)
	}
}
