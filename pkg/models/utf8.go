package models

import "unicode/utf8"

// TruncateUTF8 returns at most limit bytes of text without splitting a rune.
func TruncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if limit <= 0 {
		return ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
