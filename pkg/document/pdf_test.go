package document

import (
	"bytes"
	"testing"
)

func TestPlainTextPDFRendererIsDeterministicAndPaged(t *testing.T) {
	body := "Source: https://forum.example/thread/7\nFinding: onboarding is difficult.\n"
	for range 80 {
		body += "Evidence-backed observation with enough content to require another page.\n"
	}
	renderer := PlainTextPDFRenderer{}
	first, err := renderer.RenderPDF(Report{Title: "Market research", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderer.RenderPDF(Report{Title: "Market research", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages < 2 || !bytes.Equal(first.Bytes, second.Bytes) || !bytes.HasPrefix(first.Bytes, []byte("%PDF-1.4")) || !bytes.Contains(first.Bytes, []byte("https://forum.example/thread/7")) || !bytes.HasSuffix(first.Bytes, []byte("%%EOF\n")) {
		t.Fatalf("rendered PDF pages=%d bytes=%d", first.Pages, len(first.Bytes))
	}
	definition := SkillDefinition()
	if definition.ID != SkillID || definition.Actions[RenderPDF].EmittedArtifactTypes[0] != "report" {
		t.Fatalf("skill definition=%#v", definition)
	}
	if definition.Transport.Kind != "tool" || definition.Transport.Endpoint != SkillID {
		t.Fatalf("transport = %#v, want tool transport targeting %q", definition.Transport, SkillID)
	}
}

func TestPlainTextPDFRendererFailsExplicitlyForUnsupportedUnicode(t *testing.T) {
	_, err := (PlainTextPDFRenderer{}).RenderPDF(Report{Title: "Research", Body: "Customer said: 🙂"})
	if err == nil {
		t.Fatal("unsupported Unicode was silently corrupted")
	}
}
