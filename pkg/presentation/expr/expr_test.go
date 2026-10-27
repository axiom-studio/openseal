package expr

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
)

type fixtureFile struct {
	Tolerance float64 `json:"tolerance"`
	Cases     []struct {
		Name   string             `json:"name"`
		Expr   string             `json:"expr"`
		Vars   map[string]string  `json:"vars"`
		Inputs map[string]float64 `json:"inputs"`
		Result float64            `json:"result"`
	} `json:"cases"`
	Errors []struct {
		Name   string             `json:"name"`
		Expr   string             `json:"expr"`
		Vars   map[string]string  `json:"vars"`
		Inputs map[string]float64 `json:"inputs"`
		Phase  Phase              `json:"phase"`
	} `json:"errors"`
}

func loadFixtures(t *testing.T) fixtureFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures fixtureFile
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if fixtures.Tolerance <= 0 || len(fixtures.Cases) == 0 || len(fixtures.Errors) == 0 {
		t.Fatalf("fixtures are incomplete: %+v", fixtures)
	}
	return fixtures
}

func types(t *testing.T, vars map[string]string) map[string]Type {
	t.Helper()
	declared := map[string]Type{}
	for name, kind := range vars {
		switch kind {
		case "number":
			declared[name] = Number
		case "bool":
			declared[name] = Bool
		default:
			t.Fatalf("unknown fixture type %q", kind)
		}
	}
	return declared
}

func TestFixtureCases(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			program, err := Compile(fixture.Expr, types(t, fixture.Vars))
			if err != nil {
				t.Fatalf("compile %q: %v", fixture.Expr, err)
			}
			got, err := program.Eval(fixture.Inputs)
			if err != nil {
				t.Fatalf("eval %q: %v", fixture.Expr, err)
			}
			if math.Abs(got-fixture.Result) > fixtures.Tolerance*math.Max(1, math.Abs(fixture.Result)) {
				t.Fatalf("%q = %v, want %v", fixture.Expr, got, fixture.Result)
			}
		})
	}
}

func TestFixtureErrors(t *testing.T) {
	fixtures := loadFixtures(t)
	for _, fixture := range fixtures.Errors {
		t.Run(fixture.Name, func(t *testing.T) {
			program, err := Compile(fixture.Expr, types(t, fixture.Vars))
			if err == nil {
				if fixture.Phase != PhaseEval {
					t.Fatalf("%q compiled; want a %s error", fixture.Expr, fixture.Phase)
				}
				_, err = program.Eval(fixture.Inputs)
			}
			var failure *Error
			if !errors.As(err, &failure) || failure.Phase != fixture.Phase {
				t.Fatalf("%q error = %v, want phase %s", fixture.Expr, err, fixture.Phase)
			}
		})
	}
}

func TestExactIntegerPowersAndRounding(t *testing.T) {
	program, err := Compile("(1 + r) ^ 120", map[string]Type{"r": Number})
	if err != nil {
		t.Fatal(err)
	}
	got, err := program.Eval(map[string]float64{"r": 0.05 / 12})
	if err != nil {
		t.Fatal(err)
	}
	// Binary exponentiation, not math.Pow, so a JavaScript port matches bit for bit.
	want, base := 1.0, 1+0.05/12
	for e := 120; e > 0; {
		if e&1 == 1 {
			want *= base
		}
		if e >>= 1; e > 0 {
			base *= base
		}
	}
	if got != want {
		t.Fatalf("integer power = %v, want exactly %v", got, want)
	}
}

func TestCompileRejectsBadInputDeclarations(t *testing.T) {
	for name, inputs := range map[string]map[string]Type{
		"function name": {"sqrt": Number},
		"bad name":      {"2x": Number},
		"no type":       {"x": 0},
	} {
		if _, err := Compile("1", inputs); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestErrorsArePositioned(t *testing.T) {
	_, err := Compile("1 + y", map[string]Type{"x": Number})
	var failure *Error
	if !errors.As(err, &failure) || failure.Pos != 4 || !strings.Contains(failure.Message, "inputs are x") {
		t.Fatalf("error = %#v", err)
	}
}

func TestNodeAndNestingLimits(t *testing.T) {
	if _, err := Compile(strings.Repeat("(", 40)+"1"+strings.Repeat(")", 40), nil); err == nil {
		t.Fatal("deep nesting compiled")
	}
	if _, err := Compile(strings.TrimSuffix(strings.Repeat("1+", 200), "+"), nil); err == nil {
		t.Fatal("oversized expression compiled")
	}
	if _, err := Compile("sum(i, 1, 600, i * x)", map[string]Type{"x": Number}); err != nil {
		t.Fatal(err)
	}
}
