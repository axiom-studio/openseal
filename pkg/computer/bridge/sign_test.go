package bridge

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"regexp"
	"testing"
	"time"
)

func signedHeader(t *testing.T, now time.Time) (http.Header, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	Sign(header, private, "computer-1", http.MethodPost, PathResult, []byte(`{"id":"c1","ok":true}`), now)
	return header, public, private
}

func TestVerifyAcceptsSignedRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	header, public, _ := signedHeader(t, now)
	if SignedComputer(header) != "computer-1" {
		t.Fatalf("computer = %q", SignedComputer(header))
	}
	if err := Verify(header, public, "post", PathResult, []byte(`{"id":"c1","ok":true}`), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsTamperedRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	header, public, _ := signedHeader(t, now)
	body := []byte(`{"id":"c1","ok":true}`)
	cases := map[string]func() error{
		"body": func() error {
			return Verify(header, public, http.MethodPost, PathResult, []byte(`{"id":"c2","ok":true}`), now)
		},
		"path":   func() error { return Verify(header, public, http.MethodPost, PathNext, body, now) },
		"method": func() error { return Verify(header, public, http.MethodGet, PathResult, body, now) },
		"timestamp": func() error {
			moved := header.Clone()
			moved.Set(HeaderTimestamp, "1800000001")
			return Verify(moved, public, http.MethodPost, PathResult, body, now)
		},
	}
	for name, verify := range cases {
		if err := verify(); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("tampered %s: err = %v", name, err)
		}
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	header, _, _ := signedHeader(t, now)
	other, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(header, other, http.MethodPost, PathResult, []byte(`{"id":"c1","ok":true}`), now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("err = %v", err)
	}
	if err := Verify(header, ed25519.PublicKey("short"), http.MethodPost, PathResult, []byte(`{"id":"c1","ok":true}`), now); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("short key err = %v", err)
	}
}

func TestVerifyRejectsClockSkew(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	header, public, _ := signedHeader(t, now)
	body := []byte(`{"id":"c1","ok":true}`)
	for _, at := range []time.Time{now.Add(MaxClockSkew), now.Add(-MaxClockSkew)} {
		if err := Verify(header, public, http.MethodPost, PathResult, body, at); err != nil {
			t.Fatalf("at the skew limit: %v", err)
		}
	}
	for _, at := range []time.Time{now.Add(MaxClockSkew + time.Second), now.Add(-MaxClockSkew - time.Second)} {
		if err := Verify(header, public, http.MethodPost, PathResult, body, at); !errors.Is(err, ErrStaleTimestamp) {
			t.Fatalf("skewed err = %v", err)
		}
	}
}

func TestVerifyRejectsUnsignedRequest(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(http.Header{}, public, http.MethodGet, PathNext, nil, time.Now()); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("err = %v", err)
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(nil)
	parsed, err := ParsePublicKey(EncodePublicKey(public))
	if err != nil || !parsed.Equal(public) {
		t.Fatalf("parsed = %x %v", parsed, err)
	}
	if _, err := ParsePublicKey("bm90IGEga2V5"); err == nil {
		t.Fatal("accepted a short key")
	}
}

func TestUserCodes(t *testing.T) {
	format := regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := NewUserCode()
		if err != nil || !format.MatchString(code) {
			t.Fatalf("code = %q %v", code, err)
		}
		seen[code] = true
		if normalized, ok := NormalizeUserCode(code); !ok || normalized != code {
			t.Fatalf("normalize(%q) = %q %v", code, normalized, ok)
		}
	}
	if len(seen) < 199 {
		t.Fatalf("codes repeat: %d unique of 200", len(seen))
	}
	if got, ok := NormalizeUserCode(" abcd 2345 "); !ok || got != "ABCD-2345" {
		t.Fatalf("normalize typed = %q %v", got, ok)
	}
	for _, bad := range []string{"ABCD-234", "ABCD-2301", "ABCD-23456", "ÀBCD-2345"} {
		if got, ok := NormalizeUserCode(bad); ok {
			t.Fatalf("normalize(%q) = %q", bad, got)
		}
	}
}

func TestDeviceCodeHash(t *testing.T) {
	code, err := NewDeviceCode()
	if err != nil || len(code) < 40 {
		t.Fatalf("device code = %q %v", code, err)
	}
	if HashDeviceCode(code) != HashDeviceCode(code) || len(HashDeviceCode(code)) != 64 || HashDeviceCode(code) == HashDeviceCode(code+"x") {
		t.Fatal("device code hash is not a stable sha256")
	}
}
