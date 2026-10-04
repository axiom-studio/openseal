package skillgrpc

import "github.com/axiom-studio/openseal/pkg/skillerror"

// ActionError is the sanitized portable source failure shared with the kernel.
type ActionError = skillerror.ActionError

// NewActionError retains only recognized bounded failure facts. It returns nil
// for unknown codes, preserving existing non-browser error handling.
func NewActionError(code, message string, details map[string]string) *ActionError {
	return skillerror.NewActionError(code, message, details)
}
