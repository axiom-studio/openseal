package attachments

import (
	"archive/zip"
	"bytes"
	"testing"
)

func TestDOCXTextReadsParagraphsWithoutExternalResources(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	body, _ := writer.Create("word/document.xml")
	body.Write([]byte(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Hello &amp; Kev</w:t></w:r></w:p><w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p></w:body></w:document>`))
	link, _ := writer.Create("word/_rels/document.xml.rels")
	link.Write([]byte(`<external href="http://untrusted.invalid"/>`))
	writer.Close()
	status, text := DOCXText(t.Context(), buffer.Bytes())
	if status != "supplied" || text != "Hello & Kev\nSecond paragraph" {
		t.Fatalf("%s %q", status, text)
	}
}
func TestDOCXTextBoundsDecompression(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	body, _ := writer.Create("word/document.xml")
	body.Write(bytes.Repeat([]byte("x"), 5<<20))
	writer.Close()
	if status, _ := DOCXText(t.Context(), buffer.Bytes()); status != "size_limit" {
		t.Fatal(status)
	}
}
