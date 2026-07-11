package commands

import "testing"

func TestStandaloneOperatorRequiresExplicitLoopbackAddress(t *testing.T) {
	for address, expected := range map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		":8080":          false,
		"0.0.0.0:8080":   false,
	} {
		if actual := isLoopbackListenAddress(address); actual != expected {
			t.Fatalf("isLoopbackListenAddress(%q) = %t, want %t", address, actual, expected)
		}
	}
}
