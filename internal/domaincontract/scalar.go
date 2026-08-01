// Package domaincontract defines provider-neutral scalar contracts shared by
// canonical domain types, schema projection, and runtime validation.
package domaincontract

// StringVocabulary is implemented by a canonical string domain type whose
// complete finite vocabulary is closed and versioned with that type.
type StringVocabulary interface {
	ContractValues() []string
}

// StringFormat is implemented by a canonical string domain type with a
// structural pattern. Zero means no minimum-length constraint.
type StringFormat interface {
	ContractPattern() string
	ContractMinLength() uint64
}

func Allows(value string, vocabulary StringVocabulary) bool {
	for _, allowed := range vocabulary.ContractValues() {
		if value == allowed {
			return true
		}
	}
	return false
}
