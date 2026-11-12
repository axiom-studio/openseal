package models

import (
	"testing"
)

func TestNormalizeCredentialRequestWebsite(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.amazon.in":      "https://www.amazon.in",
		"https://WWW.Amazon.in/":     "https://www.amazon.in",
		"https://shop.example:443":   "https://shop.example",
		"http://localhost:8080":      "http://localhost:8080",
		" https://www.flipkart.com ": "https://www.flipkart.com",
	} {
		if got, err := NormalizeCredentialRequestWebsite(input); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "amazon.in", "ftp://amazon.in", "https://amazon.in/ap/signin", "https://amazon.in?x=1", "https://user:pw@amazon.in"} {
		if _, err := NormalizeCredentialRequestWebsite(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
}
