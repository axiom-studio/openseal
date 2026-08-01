package runbook

import "testing"

type finiteStringContract interface {
	ContractValues() []string
	Valid() bool
}

func TestFiniteScalarContractsAcceptExactlyTheirVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		make   func(string) finiteStringContract
	}{
		{"trigger kind", TriggerKind("").ContractValues(), func(value string) finiteStringContract { return TriggerKind(value) }},
		{"reporting milestone", ReportingMilestone("").ContractValues(), func(value string) finiteStringContract { return ReportingMilestone(value) }},
		{"step kind", StepKind("").ContractValues(), func(value string) finiteStringContract { return StepKind(value) }},
		{"delegate mode", DelegateMode("").ContractValues(), func(value string) finiteStringContract { return DelegateMode(value) }},
		{"join mode", JoinMode("").ContractValues(), func(value string) finiteStringContract { return JoinMode(value) }},
		{"predicate operator", PredicateOperator("").ContractValues(), func(value string) finiteStringContract { return PredicateOperator(value) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.values) == 0 {
				t.Fatal("contract vocabulary is empty")
			}
			for _, value := range test.values {
				if !test.make(value).Valid() {
					t.Fatalf("declared contract value %q is rejected", value)
				}
			}
			if test.make("not-a-contract-value").Valid() {
				t.Fatal("undeclared contract value was accepted")
			}
		})
	}
}

func TestJSONPointerContractMatchesRuntimeValidation(t *testing.T) {
	for _, value := range []JSONPointer{"/", "/input/value", "/steps/action/result"} {
		if !value.Valid() {
			t.Fatalf("declared JSON Pointer %q is rejected", value)
		}
	}
	for _, value := range []JSONPointer{"", "input/value"} {
		if value.Valid() {
			t.Fatalf("invalid JSON Pointer %q was accepted", value)
		}
	}
}
