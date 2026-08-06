package bundle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const signatureContext = "openseal.workforce-bundle.v1\x00"

type TrustPolicy struct {
	RequireSignature bool
	TrustedKeys      map[string]ed25519.PublicKey
}

type Verification struct {
	Digest             string   `json:"digest"`
	ValidSignatureKeys []string `json:"validSignatureKeys"`
	TrustedKeys        []string `json:"trustedKeys,omitempty"`
	Trusted            bool     `json:"trusted"`
}

func Sign(bundle *Bundle, keyID string, privateKey ed25519.PrivateKey) error {
	if bundle == nil || !portableIDPattern.MatchString(strings.TrimSpace(keyID)) || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("workforce bundle signing requires a portable key id and Ed25519 private key")
	}
	if err := bundle.Validate(); err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	signature := Signature{
		Algorithm: "ed25519", KeyID: keyID,
		PublicKey: base64.RawStdEncoding.EncodeToString(publicKey),
		Value:     base64.RawStdEncoding.EncodeToString(ed25519.Sign(privateKey, signaturePayload(bundle.Digest))),
	}
	replaced := false
	for index := range bundle.Signatures {
		if bundle.Signatures[index].KeyID == keyID {
			bundle.Signatures[index], replaced = signature, true
		}
	}
	if !replaced {
		bundle.Signatures = append(bundle.Signatures, signature)
	}
	canonicalize(bundle)
	return nil
}

func Verify(bundle *Bundle, policy TrustPolicy) (*Verification, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	if policy.RequireSignature && len(bundle.Signatures) == 0 {
		return nil, errors.New("workforce bundle requires a signature")
	}
	result := &Verification{Digest: bundle.Digest}
	for _, signature := range bundle.Signatures {
		publicKey, err := decodePublicKey(signature.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("workforce bundle signature %s: %w", signature.KeyID, err)
		}
		value, err := base64.RawStdEncoding.DecodeString(signature.Value)
		if err != nil || len(value) != ed25519.SignatureSize || !ed25519.Verify(publicKey, signaturePayload(bundle.Digest), value) {
			return nil, fmt.Errorf("workforce bundle signature %s is invalid", signature.KeyID)
		}
		result.ValidSignatureKeys = append(result.ValidSignatureKeys, signature.KeyID)
		if trusted, ok := policy.TrustedKeys[signature.KeyID]; ok && len(trusted) == ed25519.PublicKeySize && subtle.ConstantTimeCompare(trusted, publicKey) == 1 {
			result.TrustedKeys = append(result.TrustedKeys, signature.KeyID)
		}
	}
	sort.Strings(result.ValidSignatureKeys)
	sort.Strings(result.TrustedKeys)
	result.Trusted = len(result.TrustedKeys) > 0
	if len(policy.TrustedKeys) > 0 && !result.Trusted {
		return result, errors.New("workforce bundle has no signature from a trusted key")
	}
	return result, nil
}

func validateSignatureShapes(signatures []Signature) error {
	seen := map[string]bool{}
	for _, signature := range signatures {
		if signature.Algorithm != "ed25519" || !portableIDPattern.MatchString(signature.KeyID) || seen[signature.KeyID] {
			return errors.New("workforce bundle signatures require unique portable key ids and Ed25519")
		}
		if _, err := decodePublicKey(signature.PublicKey); err != nil {
			return fmt.Errorf("workforce bundle signature %s: %w", signature.KeyID, err)
		}
		value, err := base64.RawStdEncoding.DecodeString(signature.Value)
		if err != nil || len(value) != ed25519.SignatureSize {
			return fmt.Errorf("workforce bundle signature %s has invalid encoding", signature.KeyID)
		}
		seen[signature.KeyID] = true
	}
	return nil
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	value, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(value) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	return ed25519.PublicKey(value), nil
}

func signaturePayload(digest string) []byte {
	hashed := sha256.Sum256([]byte(signatureContext + digest))
	return hashed[:]
}
