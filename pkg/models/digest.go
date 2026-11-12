package models

import (
	"crypto/sha256"
	"encoding/hex"
)

func SHA256DigestString(value string) string { return SHA256Digest([]byte(value)) }

func SHA256Digest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
