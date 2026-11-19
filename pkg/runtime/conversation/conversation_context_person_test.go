package conversation

import (
	"strings"
	"testing"
	"time"
)

func TestRunContextPersonGivesProfileAndLocalTime(t *testing.T) {
	now := time.Date(2026, 11, 19, 14, 3, 0, 0, time.UTC)
	text := RunContextPerson(Person{Email: "kev@example.com", Name: "Plouton", Company: "Axiom", Timezone: "Asia/Kolkata"}, now)
	for _, want := range []string{"Account email: kev@example.com", "Name: Plouton", "Company: Axiom", "Asia/Kolkata", "Thursday 19 November 2026, 7:33 PM (UTC+05:30)", "at 2pm"} {
		if !strings.Contains(text, want) {
			t.Errorf("person section lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Phone") {
		t.Errorf("an empty field was shown:\n%s", text)
	}
	if got := RunContextPerson(Person{Email: "kev@example.com"}, now); !strings.Contains(got, "Timezone: not known") {
		t.Errorf("without a timezone = %q", got)
	}
	if got := RunContextPerson(Person{}, now); got != "" {
		t.Errorf("nobody behind the run = %q", got)
	}
}
