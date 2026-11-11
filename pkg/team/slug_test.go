package team

import (
	"strings"
	"testing"
)

func TestMemberSlug(t *testing.T) {
	for name, want := range map[string]string{
		"Inbox Triage":            "inbox-triage",
		"  Ops -- Crew!! ":        "ops-crew",
		"Zoë 2":                   "zo-2",
		"日本":                      "member",
		"":                        "member",
		strings.Repeat("ab ", 30): strings.Trim(strings.Repeat("ab-", 16), "-"),
	} {
		if got := MemberSlug(name); got != want {
			t.Errorf("MemberSlug(%q) = %q, want %q", name, got, want)
		}
	}
}
