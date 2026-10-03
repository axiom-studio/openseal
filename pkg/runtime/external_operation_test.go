package runtime

import (
	"strings"
	"testing"
)

func TestExternalOperationLockKeyIsStablePrintableAndScopeSeparated(t *testing.T) {
	digest := hashString("stable operation")
	first := externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, digest)
	if first != externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, digest) {
		t.Fatal("lock key is not stable")
	}
	if strings.ContainsRune(first, '\x00') || len(first) != 71 || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("lock key is not a printable SHA-256 digest: %q", first)
	}
	if first == externalOperationLockKey(Scope{Kind: "tenant", ID: "2"}, digest) ||
		first == externalOperationLockKey(Scope{Kind: "tenant", ID: "1"}, hashString("other operation")) {
		t.Fatal("lock key does not separate scope and operation identity")
	}
}

func TestExternalOperationCanonicalizesProviderChannelHandle(t *testing.T) {
	identity := ExternalOperationIdentity{Resource: " #Agents ", Operation: "message:send"}
	if err := identity.Validate(); err != nil {
		t.Fatalf("validate provider channel handle: %v", err)
	}
	resource, err := canonicalExternalOperationResource(identity.Resource)
	if err != nil {
		t.Fatalf("canonicalize provider channel handle: %v", err)
	}
	if resource != "channel-handle:agents" {
		t.Fatalf("canonical resource = %q", resource)
	}
}

func TestExternalOperationCanonicalizesSingleMailbox(t *testing.T) {
	for _, input := range []string{
		"Alice+alerts@EXAMPLE.COM",
		"mailto:Alice+alerts@example.com",
		"MAILTO:Alice+alerts@Example.Com",
		" Alice+alerts@example.com ",
	} {
		t.Run(input, func(t *testing.T) {
			resource, err := canonicalExternalOperationResource(input)
			if err != nil || resource != "mailto:Alice+alerts@example.com" {
				t.Fatalf("canonical mailbox = %q, err = %v", resource, err)
			}
			if canonical, err := canonicalExternalOperationResource(resource); err != nil || canonical != resource {
				t.Fatalf("canonical mailbox is not stable: %q, err = %v", canonical, err)
			}
		})
	}
}

func TestExternalOperationMailboxDigestDeduplicatesEquivalentForms(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "1"}
	digest := func(resource, occurrence string) string {
		t.Helper()
		result, err := computeExternalOperationDigest(scope, ObjectiveOwner{}, "objective", &ExternalOperationIdentity{
			Resource: resource, Operation: "email:send",
		}, occurrence)
		if err != nil {
			t.Fatalf("digest %q: %v", resource, err)
		}
		return result
	}
	first := digest("Alice+alerts@EXAMPLE.COM", "message-1")
	if first != digest("mailto:Alice+alerts@example.com", "message-1") {
		t.Fatal("bare and mailto forms must protect the same delivery occurrence")
	}
	for _, resource := range []string{"alice+alerts@example.com", "Alice@example.com", "Bob+alerts@example.com", "Alice+alerts@example.org"} {
		if first == digest(resource, "message-1") {
			t.Fatalf("different mailbox %q shares an operation digest", resource)
		}
	}
	if first == digest("Alice+alerts@example.com", "message-2") {
		t.Fatal("separately reviewed deliveries must retain distinct occurrence identities")
	}
}

func TestExternalOperationRejectsMalformedOrAnnotatedMailbox(t *testing.T) {
	for _, input := range []string{
		"mailto:", "mailto:not-an-address", "MAILTO:not-an-address",
		"alice@", "@example.com", "alice@@example.com",
		"Alice <alice@example.com>", "<alice@example.com>", "alice@example.com (Alice)",
		"send to alice@example.com", "mailto: alice@example.com",
		"alice@example.com,bob@example.com", "mailto:alice@example.com;bob@example.com",
		"mailto:alice@example.com?subject=hello", "mailto:alice@example.com?cc=bob@example.com",
		"mailto:alice@example.com#fragment", "alice@example.com?body=hello",
		"mailto:alice@example.com%0d%0aBcc:bob@example.com",
		"mailto:alice%40example.com", "alice:password@example.com",
		"alice@example.com\r\nBcc:bob@example.com", "\nalice@example.com",
		"alice@example.com\t", "alice\x00@example.com", "alice\x7f@example.com",
		"alice @example.com", "alice@ example.com",
		"alice@example.com/other", "alice@example.com\\other",
		strings.Repeat("a", 2036) + "@example.com",
	} {
		t.Run(input, func(t *testing.T) {
			identity := ExternalOperationIdentity{Resource: input, Operation: "email:send"}
			if err := identity.Validate(); err == nil {
				t.Fatal("unsafe or malformed mailbox was accepted")
			}
		})
	}
}

func TestExternalOperationMailboxSupportPreservesOtherResourceValidation(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"production-release", "production-release"},
		{"browser-session:one", "browser-session:one"},
		{" #Agents ", "channel-handle:agents"},
		{"https://EXAMPLE.COM/users/alice@example.com?b=2&a=1#section", "https://example.com/users/alice@example.com?a=1&b=2"},
	} {
		resource, err := canonicalExternalOperationResource(tc.input)
		if err != nil || resource != tc.want {
			t.Fatalf("resource %q canonicalized to %q, err = %v", tc.input, resource, err)
		}
	}
	for _, input := range []string{
		"https://alice:password@example.com/mail", "https://example.com/mail?access_token=secret",
		"opaque/identifier", "mailto:alice@example.com/other",
	} {
		if _, err := canonicalExternalOperationResource(input); err == nil {
			t.Fatalf("invalid resource %q was accepted", input)
		}
	}
}
