package authoring

import "testing"

type refinementFiniteStringContract interface {
	ContractValues() []string
	Valid() bool
}

func TestRefinementScalarContractsAcceptExactlyTheirVocabulary(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		make   func(string) refinementFiniteStringContract
	}{
		{"category", RefinementQuestionCategory("").ContractValues(), func(value string) refinementFiniteStringContract { return RefinementQuestionCategory(value) }},
		{"answer kind", RefinementAnswerKind("").ContractValues(), func(value string) refinementFiniteStringContract { return RefinementAnswerKind(value) }},
		{"blocking scope", RefinementBlockingScope("").ContractValues(), func(value string) refinementFiniteStringContract { return RefinementBlockingScope(value) }},
		{"provenance kind", RefinementProvenanceKind("").ContractValues(), func(value string) refinementFiniteStringContract { return RefinementProvenanceKind(value) }},
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
