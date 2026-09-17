package attachments

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaximumTextBytes = 64 * 1024
	MaximumFileBytes = 4 * 1024 * 1024
)

// PDFText extracts text only. Authorization and persisted-content integrity
// verification must happen before calling this parser. It neither runs OCR nor
// follows embedded links. Poppler is a host dependency, not model-selected code.
func PDFText(ctx context.Context, content []byte) (string, string) {
	if len(content) > MaximumFileBytes {
		return "size_limit", ""
	}
	if !bytes.HasPrefix(content, []byte("%PDF-")) {
		return "unreadable", ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Both binaries must be supplied by the host. Resource limits apply to the
	// untrusted-document parser, not to the long-lived agent process.
	parser, err := exec.LookPath("pdftotext")
	if err != nil {
		return "unreadable", ""
	}
	limiter, err := exec.LookPath("prlimit")
	if err != nil {
		return "unreadable", ""
	}
	cmd := exec.CommandContext(ctx, limiter, "--as=536870912", "--cpu=5", "--fsize=0", "--", parser, "-layout", "-nopgbrk", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(content)
	output := &boundedPDFOutput{limit: MaximumTextBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard // Diagnostics can contain private document data.
	err = cmd.Run()
	if output.exceeded {
		return "size_limit", ""
	}
	if err != nil || ctx.Err() != nil {
		return "unreadable", ""
	}
	if !utf8.Valid(output.Bytes()) || bytes.ContainsRune(output.Bytes(), '\x00') {
		return "unreadable", ""
	}
	text := strings.TrimSpace(output.String())
	if text == "" {
		return "no_extractable_text", ""
	}
	return "supplied", text
}

type boundedPDFOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *boundedPDFOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		b.exceeded = true
		return 0, errors.New("PDF text exceeds limit")
	}
	return b.Buffer.Write(p)
}
