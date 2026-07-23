package runtime

import "errors"

// ErrLeaseLost reports that a durable canonical Run, turn, or action lease is
// no longer owned by the caller.
var ErrLeaseLost = errors.New("run lease lost")
