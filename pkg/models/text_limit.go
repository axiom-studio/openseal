package models

import "unicode/utf8"

// TruncateRunes shortens value to at most limit runes, ending a shortened
// value with an ellipsis. It counts runes, not bytes, so text shown to
// people and models is never cut inside a character.
func TruncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit-1]) + "…"
}
