package variant

import (
	"net/http"
	"strings"
	"testing"
)

func TestIdentityConstruction(t *testing.T) {
	headers := http.Header{
		"Accept-Encoding": {" gzip "},
		"ACCEPT-ENCODING": {"br"},
		"X-Lang":          {"en", "fr"},
		"X-Empty":         {"  "},
	}

	cases := []struct {
		name       string
		noVary     bool
		vary       []string
		wantNoVary bool
		wantVary   []string
	}{
		{
			name:       "no-vary directive beats a real Vary",
			noVary:     true,
			vary:       []string{"X-Lang"},
			wantNoVary: true,
		},
		{
			name:       "empty set is a single representation",
			wantNoVary: true,
		},
		{
			name:       "star is a single representation",
			vary:       []string{"X-Lang", "*"},
			wantNoVary: true,
		},
		{
			name:     "names are trimmed, lowercased, deduplicated and sorted",
			vary:     []string{" X-LANG ", "x-lang", "", "Accept-Encoding, X-Empty"},
			wantVary: []string{"accept-encoding", "x-empty", "x-lang"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := New(tc.noVary, tc.vary, headers)
			if identity.NoVary() != tc.wantNoVary {
				t.Fatalf("NoVary = %v, want %v", identity.NoVary(), tc.wantNoVary)
			}
			if got := strings.Join(identity.Vary(), ","); got != strings.Join(tc.wantVary, ",") {
				t.Fatalf("Vary = %v, want %v", identity.Vary(), tc.wantVary)
			}
			if tc.wantNoVary {
				if len(identity.RequestHeaders()) != 0 {
					t.Fatalf("no-vary retained request headers: %v", identity.RequestHeaders())
				}
				return
			}
			// Merged in sorted original-name order: ACCEPT-ENCODING before
			// Accept-Encoding, each trimmed, empty values dropped.
			if value, present := identity.Value("accept-encoding"); !present || value != "br, gzip" {
				t.Fatalf("accept-encoding = %q/%v, want %q", value, present, "br, gzip")
			}
			if value, present := identity.Value("x-lang"); !present || value != "en, fr" {
				t.Fatalf("x-lang = %q/%v, want %q", value, present, "en, fr")
			}
			if value, present := identity.Value("x-empty"); !present || value != "" {
				t.Fatalf("x-empty = %q/%v, want present and empty", value, present)
			}
			if _, present := identity.Value("x-missing"); present {
				t.Fatal("an unselected header was retained")
			}
		})
	}
}

func TestIdentityMergeIsDeterministic(t *testing.T) {
	h := http.Header{
		"Accept-Encoding": {" gzip "},
		"ACCEPT-ENCODING": {"br"},
	}
	for i := 0; i < 50; i++ {
		got := New(false, []string{"accept-encoding"}, h).RequestHeaders()
		if values := got["accept-encoding"]; len(values) != 1 || values[0] != "br, gzip" {
			t.Fatalf("iteration %d: %v, want %q", i, values, "br, gzip")
		}
	}
}

func TestIdentityPresenceIsPartOfIdentity(t *testing.T) {
	absent := New(false, []string{"X-A"}, http.Header{})
	empty := New(false, []string{"X-A"}, http.Header{"x-a": {""}})
	spaces := New(false, []string{"X-A"}, http.Header{"X-A": {"   "}})

	if absent.Equal(empty) || absent.Equal(spaces) {
		t.Fatal("an absent header equalled a present-but-empty one")
	}
	if !empty.Equal(spaces) {
		t.Fatal("whitespace-only values should normalise to one present-empty identity")
	}

	if !absent.Matches(http.Header{}) {
		t.Error("absent identity did not match a request without the header")
	}
	if absent.Matches(http.Header{"x-a": {""}}) {
		t.Error("absent identity matched a present-but-empty request")
	}
	if !empty.Matches(http.Header{"X-A": {""}}) || !empty.Matches(http.Header{"x-a": {"  "}}) {
		t.Error("present-empty identity did not match a present-but-empty request")
	}
	if empty.Matches(http.Header{}) {
		t.Error("present-empty identity matched a request without the header")
	}
}

func TestIdentityEquality(t *testing.T) {
	base := New(false, []string{"X-B", "x-a"}, http.Header{"x-a": {"1"}, "x-b": {"2"}})

	same := New(false, []string{"x-a", "X-B", "x-a"}, http.Header{"X-A": {" 1 "}, "X-B": {"2"}})
	if !base.Equal(same) {
		t.Fatal("case, order and duplication changed the identity")
	}

	otherValue := New(false, []string{"x-a", "x-b"}, http.Header{"x-a": {"1"}, "x-b": {"3"}})
	if base.Equal(otherValue) {
		t.Fatal("a different matched value compared equal")
	}

	subset := New(false, []string{"x-a"}, http.Header{"x-a": {"1"}})
	if base.Equal(subset) {
		t.Fatal("a different Vary set compared equal")
	}

	if base.Equal(New(true, nil, nil)) {
		t.Fatal("a varied identity equalled the single-representation case")
	}
	if !New(true, nil, nil).Equal(New(false, nil, nil)) {
		t.Fatal("no-vary and an empty Vary set should be one identity")
	}
}

func TestIdentityExcluding(t *testing.T) {
	base := New(false, []string{"Accept-Encoding", "X-Lang"}, http.Header{
		"Accept-Encoding": {"gzip"},
		"X-Lang":          {"en"},
	})
	reduced := base.Excluding("ACCEPT-ENCODING")
	if got := strings.Join(reduced.Vary(), ","); got != "x-lang" {
		t.Fatalf("Vary after excluding = %q, want x-lang", got)
	}
	if value, present := reduced.Value("x-lang"); !present || value != "en" {
		t.Fatalf("x-lang lost while excluding a sibling: %q/%v", value, present)
	}
	if _, present := reduced.Value("accept-encoding"); present {
		t.Fatal("excluded header value survived")
	}

	only := New(false, []string{"Accept-Encoding"}, http.Header{"accept-encoding": {"gzip"}})
	if folded := only.Excluding("accept-encoding"); !folded.NoVary() {
		t.Fatal("excluding the only dimension should leave a single representation")
	}
	if !base.Excluding("x-unrelated").Equal(base) {
		t.Fatal("excluding an absent dimension changed the identity")
	}
}
