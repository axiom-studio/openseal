package hostadapter

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/axiom-studio/openseal/pkg/document"
)

func TestDocumentPDFExecutorReturnsTransientBytes(t *testing.T) {
	executor := NewDocumentPDFExecutor(document.PlainTextPDFRenderer{})
	result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{
		"title": "Research", "body": "Source: https://forum.example/thread/7", "filename": "research.pdf", "requirementName": "report",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(result.Output["_transientPdfBase64"].(string))
	if err != nil || string(raw[:8]) != "%PDF-1.4" || result.Output["pages"] != 1 {
		t.Fatalf("PDF output=%#v err=%v", result.Output, err)
	}
}
