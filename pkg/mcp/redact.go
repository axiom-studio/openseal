package mcp

import (
	"sort"
	"strings"
)

const redacted = "[REDACTED]"

// redactor scrubs one session's secret values, longest first so a secret
// that contains another is removed whole.
type redactor []string

func newRedactor(values ...string) redactor {
	r := redactor{}
	for _, v := range values {
		if v != "" {
			r = append(r, v)
		}
	}
	sort.Slice(r, func(i, j int) bool { return len(r[i]) > len(r[j]) })
	return r
}

func (r redactor) string(s string) string {
	for _, secret := range r {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// value returns a scrubbed copy. maskKeys also masks values under
// credential-like keys; tool definitions keep them so schemas stay intact.
func (r redactor) value(v interface{}, maskKeys bool) interface{} {
	switch x := v.(type) {
	case string:
		return r.string(x)
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, y := range x {
			out[i] = r.value(y, maskKeys)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, y := range x {
			value := r.value(y, maskKeys)
			if maskKeys {
				switch strings.ToLower(k) {
				case "authorization", "password", "secret", "token", "access_token", "refresh_token", "api_key", "apikey":
					value = redacted
				}
			}
			out[r.string(k)] = value
		}
		return out
	default:
		return v
	}
}
