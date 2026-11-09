package workspace

import (
	"bytes"
	"fmt"
	"sync"
	"unicode/utf8"
)

// StoppedNote ends the stderr of a command stopped at its timeout.
func StoppedNote(seconds int) string {
	return fmt.Sprintf("\nThe command was stopped after %d seconds.\n", seconds)
}

// NotStartedNote is the stderr of a command that could not start.
func NotStartedNote(err error) string {
	return fmt.Sprintf("The command could not start: %v\n", err)
}

// OutputTail keeps the last CommandOutputBytes written to it: a command's
// stdout or stderr, whose end explains how it ended.
type OutputTail struct {
	mu        sync.Mutex
	limit     int
	buf       []byte
	truncated bool
}

// NewOutputTail keeps the last limit bytes, CommandOutputBytes when limit is
// 0.
func NewOutputTail(limit int) *OutputTail {
	if limit <= 0 {
		limit = CommandOutputBytes
	}
	return &OutputTail{limit: limit}
}

func (t *OutputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.truncated = true
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

// Truncated reports whether the output lost its start.
func (t *OutputTail) Truncated() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.truncated
}

// String is the kept output as valid UTF-8; a character cut by the limit is
// dropped.
func (t *OutputTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	buf := t.buf
	if t.truncated {
		for i := 0; i < utf8.UTFMax && len(buf) > 0 && !utf8.RuneStart(buf[0]); i++ {
			buf = buf[1:]
		}
	}
	return string(bytes.ToValidUTF8(buf, []byte("�")))
}
