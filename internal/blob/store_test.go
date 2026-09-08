package blob

import (
	"errors"
	"strings"
	"testing"
)

func TestPutIsContentAddressedAndDeduplicates(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ref1, err := s.PutBytes([]byte("hello nemuz"))
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := s.PutBytes([]byte("hello nemuz"))
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 {
		t.Fatalf("identical content produced different refs: %s != %s", ref1, ref2)
	}
	if !ref1.Valid() {
		t.Fatalf("ref is not a well-formed sha256: %q", ref1)
	}

	other, err := s.PutBytes([]byte("hello nemuz!"))
	if err != nil {
		t.Fatal(err)
	}
	if other == ref1 {
		t.Fatal("different content produced the same ref")
	}
}

func TestGetRoundTrips(t *testing.T) {
	s, _ := Open(t.TempDir())
	want := strings.Repeat("payload ", 1000)

	ref, err := s.PutBytes([]byte(want))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBytes(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("content changed in the store: got %d bytes want %d", len(got), len(want))
	}
	if !s.Has(ref) {
		t.Fatal("Has reported false for a blob that was just written")
	}
}

func TestGetMissingReportsNotFound(t *testing.T) {
	s, _ := Open(t.TempDir())
	missing := Ref(strings.Repeat("a", 64))

	_, err := s.GetBytes(missing)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for an absent blob, got %v", err)
	}
	if s.Has(missing) {
		t.Fatal("Has reported true for an absent blob")
	}
}

func TestMalformedRefIsRejected(t *testing.T) {
	s, _ := Open(t.TempDir())
	for _, bad := range []Ref{"", "abc", Ref(strings.Repeat("z", 64))} {
		if bad.Valid() {
			t.Errorf("Valid accepted malformed ref %q", bad)
		}
		if _, err := s.GetBytes(bad); err == nil {
			t.Errorf("GetBytes accepted malformed ref %q", bad)
		}
	}
}
