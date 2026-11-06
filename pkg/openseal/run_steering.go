package openseal

import "github.com/axiom-studio/openseal/pkg/runtime"

// ErrRunCommandNotAuthorized refuses a user other than the person a run
// acts for when they intervene in, answer, resume or retry it.
var ErrRunCommandNotAuthorized = runtime.ErrRunCommandNotAuthorized
