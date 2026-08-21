package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, err := New("test-key-that-is-long-enough-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range []string{"", "x", "-----BEGIN RSA PRIVATE KEY-----\nabc\n", strings.Repeat("a", 5000)} {
		sealed, err := b.Seal(pt)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if pt != "" && strings.Contains(sealed, pt) {
			t.Fatalf("plaintext leaked into sealed value")
		}
		got, err := b.Open(sealed)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got != pt {
			t.Fatalf("round trip: got %q want %q", got, pt)
		}
	}
}

func TestSealIsNondeterministic(t *testing.T) {
	b, _ := New("test-key-that-is-long-enough-for-tests")
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if a == c {
		t.Fatal("two seals of the same plaintext are identical; nonce is not random")
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	b, _ := New("test-key-that-is-long-enough-for-tests")
	sealed, _ := b.Seal("secret")
	other, _ := New("a-completely-different-key-value-here")
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("expected failure opening with the wrong key")
	}
}

func TestOpenRejectsUnsealed(t *testing.T) {
	b, _ := New("test-key-that-is-long-enough-for-tests")
	if _, err := b.Open("just a plaintext token"); !errors.Is(err, ErrNotSealed) {
		t.Fatalf("got %v want ErrNotSealed", err)
	}
}
