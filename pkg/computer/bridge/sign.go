package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Signature headers of every request after pairing.
const (
	HeaderComputer  = "X-Openseal-Computer"
	HeaderTimestamp = "X-Openseal-Timestamp"
	HeaderSignature = "X-Openseal-Signature"
)

// MaxClockSkew is how far a signed timestamp may be from the server's clock.
const MaxClockSkew = 120 * time.Second

var (
	ErrUnsigned         = errors.New("request is not signed")
	ErrStaleTimestamp   = errors.New("request timestamp is outside the allowed clock skew")
	ErrInvalidSignature = errors.New("request signature is invalid")
)

// SigningPayload is what a request signature covers:
// METHOD "\n" PATH "\n" TIMESTAMP "\n" hex(sha256(body)). path is relative to
// PathPrefix, such as /next.
func SigningPayload(method, path, timestamp string, body []byte) []byte {
	return digestPayload(method, path, timestamp, BodySHA256(body))
}

func digestPayload(method, path, timestamp, bodySHA256 string) []byte {
	return []byte(strings.ToUpper(method) + "\n" + path + "\n" + timestamp + "\n" + bodySHA256)
}

// BodySHA256 is the hex sha256 a signature covers for body.
func BodySHA256(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Sign sets the signature headers of a request from computerID with key.
func Sign(header http.Header, key ed25519.PrivateKey, computerID, method, path string, body []byte, now time.Time) {
	SignDigest(header, key, computerID, method, path, BodySHA256(body), now)
}

// SignDigest is Sign for a body given by its hex sha256, for an upload
// streamed from a file rather than held in memory.
func SignDigest(header http.Header, key ed25519.PrivateKey, computerID, method, path, bodySHA256 string, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	header.Set(HeaderComputer, computerID)
	header.Set(HeaderTimestamp, timestamp)
	header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(ed25519.Sign(key, digestPayload(method, path, timestamp, bodySHA256))))
}

// SignedComputer is the computer a signed request claims to come from; the
// server looks up its key, then calls Verify.
func SignedComputer(header http.Header) string {
	return strings.TrimSpace(header.Get(HeaderComputer))
}

// Verify checks a request's signature against the computer's public key and
// rejects a timestamp more than MaxClockSkew from now.
func Verify(header http.Header, key ed25519.PublicKey, method, path string, body []byte, now time.Time) error {
	return VerifyDigest(header, key, method, path, BodySHA256(body), now, MaxClockSkew)
}

// VerifyDigest is Verify for a body given by its hex sha256, accepting a
// timestamp up to maxAge old (and at most MaxClockSkew ahead). An upload
// checks its signature with it before reading the body (MaxUploadAge).
func VerifyDigest(header http.Header, key ed25519.PublicKey, method, path, bodySHA256 string, now time.Time, maxAge time.Duration) error {
	timestamp := strings.TrimSpace(header.Get(HeaderTimestamp))
	encoded := strings.TrimSpace(header.Get(HeaderSignature))
	if SignedComputer(header) == "" || timestamp == "" || encoded == "" {
		return ErrUnsigned
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	if age := now.Sub(time.Unix(seconds, 0)); age > maxAge || age < -MaxClockSkew {
		return ErrStaleTimestamp
	}
	signature, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, digestPayload(method, path, timestamp, bodySHA256), signature) {
		return ErrInvalidSignature
	}
	return nil
}

// EncodePublicKey is the DeviceStartRequest form of a public key.
func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

// ParsePublicKey reads a DeviceStartRequest public key.
func ParsePublicKey(encoded string) (ed25519.PublicKey, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("public key must be a base64 ed25519 key")
	}
	return ed25519.PublicKey(key), nil
}

// userCodeAlphabet is A-Z and 2-9: no 0 or 1 to mistake for O or I.
const userCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ23456789"

// NewUserCode is a random pairing code a person types or confirms,
// XXXX-XXXX.
func NewUserCode() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(userCodeAlphabet)))
	for i := 0; i < 8; i++ {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(userCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// NormalizeUserCode turns what a person typed (any case, spaces, with or
// without the dash) into XXXX-XXXX, or reports it is not a code.
func NormalizeUserCode(input string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(input) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r < 128 && strings.IndexByte(userCodeAlphabet, byte(r)) >= 0:
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	code := b.String()
	if len(code) != 8 {
		return "", false
	}
	return code[:4] + "-" + code[4:], true
}

// NewDeviceCode is the secret the bridge polls pairing with. The server
// stores only HashDeviceCode of it.
func NewDeviceCode() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("device code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashDeviceCode is the stored form of a device code: hex sha256.
func HashDeviceCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
