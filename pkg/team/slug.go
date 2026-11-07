package team

import "strings"

// RoleSlug turns a member's display name into the base of a role id:
// lowercase letters and digits joined by single dashes, at most 48
// characters, or "member" when nothing is left.
func RoleSlug(name string) string {
	var out strings.Builder
	dash := false
	for _, char := range strings.ToLower(name) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			out.WriteRune(char)
			dash = false
		} else if !dash && out.Len() > 0 {
			out.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(out.String(), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	if slug == "" {
		slug = "member"
	}
	return slug
}
