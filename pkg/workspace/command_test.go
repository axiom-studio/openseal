package workspace

import "testing"

func TestOutputTailKeepsTheEndAsValidUTF8(t *testing.T) {
	out := NewOutputTail(4)
	_, _ = out.Write([]byte("abc"))
	_, _ = out.Write([]byte("dé€"))
	// "abcdé€" is 4+2+3 bytes; the last 4 are é's second byte and €.
	if got := out.String(); got != "€" || !out.Truncated() {
		t.Fatalf("tail = %q truncated %v", got, out.Truncated())
	}
}
