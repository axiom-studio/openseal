package attachments

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/document"
)

func TestPDFTextRejectsInvalidAndOversizeInput(t *testing.T) {
	for _, tc := range []struct{ input, status string }{
		{"not a PDF", "unreadable"},
		{strings.Repeat("x", MaximumFileBytes+1), "size_limit"},
	} {
		status, text := PDFText(t.Context(), []byte(tc.input))
		if status != tc.status || text != "" {
			t.Fatalf("got %s %q", status, text)
		}
	}
}

func TestBoundedPDFOutputNeverRetainsOversizeText(t *testing.T) {
	b := &boundedPDFOutput{limit: 3}
	if _, err := b.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("d")); err == nil || !b.exceeded || b.String() != "abc" {
		t.Fatal("text limit not enforced")
	}
}

func TestPDFTextHostParser(t *testing.T) {
	for _, binary := range []string{"pdftotext", "prlimit"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("host dependency unavailable: %s", binary)
		}
	}
	pdf, err := (document.PlainTextPDFRenderer{}).RenderPDF(document.Report{Title: "Attachment test", Body: "This is actual extracted document text."})
	if err != nil {
		t.Fatal(err)
	}
	status, text := PDFText(t.Context(), pdf.Bytes)
	if status != "supplied" || !strings.Contains(text, "actual extracted document text") {
		t.Fatalf("got %s %q", status, text)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if status, text := PDFText(ctx, pdf.Bytes); status != "unreadable" || text != "" {
		t.Fatal("canceled read exposed content")
	}
	if status, text := PDFText(t.Context(), []byte("%PDF-broken")); status != "unreadable" || text != "" {
		t.Fatal("malformed PDF accepted")
	}
}
