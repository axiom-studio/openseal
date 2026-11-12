package models

import "strings"

func ExternalSourceLabel(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}
