package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"
)

func spreadsheetFixture(t *testing.T, change func(map[string]string)) []byte {
	t.Helper()
	parts := map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Sales" r:id="rId1"/><sheet name="Notes" r:id="rId2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst><si><r><t>Net </t></r><r><t>sales</t></r></si></sst>`,
		"xl/worksheets/sheet1.xml":   `<worksheet><sheetData><row><c r="A1" t="s"><v>0</v></c><c r="C1"><v>42</v></c><c r="D1"><f>SUM(C1:C2)</f><v>42</v></c><c r="E1"><f>WEBSERVICE("https://example.invalid")</f></c></row></sheetData></worksheet>`,
		"xl/worksheets/sheet2.xml":   `<worksheet><sheetData><row><c r="A1" t="inlineStr"><is><t>Quarterly report</t></is></c><c r="B1" t="b"><v>1</v></c></row></sheetData></worksheet>`,
	}
	if change != nil {
		change(parts)
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, data := range parts {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestSpreadsheetAttachmentReadsValuesWithoutExecutingFormulas(t *testing.T) {
	content := spreadsheetFixture(t, nil)
	artifact := conversationTestArtifact()
	artifact.MediaType, artifact.SizeBytes, artifact.Digest = spreadsheetMediaType, int64(len(content)), conversationTestDigest(string(content))
	status, text := readConversationAttachment(t.Context(), &conversationTestContent{text: string(content)}, artifact, conversationAttachmentBudget)
	if status != "supplied" {
		t.Fatalf("status = %s", status)
	}
	for _, expected := range []string{`Sheet "Sales"`, `A1: "Net sales"`, `C1: "42"`, `D1: "42"`, `E1: "[formula has no cached value]"`, `Sheet "Notes"`, `A1: "Quarterly report"`, `B1: "true"`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("missing %s in %s", expected, text)
		}
	}
	if strings.Contains(text, "https://") {
		t.Fatal("formula was projected as data")
	}
}

func TestSpreadsheetAttachmentRejectsUnsafeOrInvalidDocuments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(map[string]string)
		limit    int
		expected string
	}{
		{"budget", nil, 20, "size_limit"},
		{"external", func(p map[string]string) {
			p["xl/_rels/workbook.xml.rels"] = `<Relationships><Relationship Id="rId1" Type="x/worksheet" Target="https://example.invalid/sheet.xml" TargetMode="External"/></Relationships>`
		}, 65536, "invalid_document"},
		{"traversal", func(p map[string]string) {
			p["xl/_rels/workbook.xml.rels"] = `<Relationships><Relationship Id="rId1" Type="x/worksheet" Target="../../private.xml"/></Relationships>`
		}, 65536, "invalid_document"},
		{"bad index", func(p map[string]string) {
			p["xl/worksheets/sheet1.xml"] = `<worksheet><sheetData><row><c r="A1" t="s"><v>999</v></c></row></sheetData></worksheet>`
		}, 65536, "invalid_document"},
		{"bad xml", func(p map[string]string) { p["xl/sharedStrings.xml"] = "<broken" }, 65536, "invalid_document"},
		{"zip bomb", func(p map[string]string) { p["xl/huge.xml"] = strings.Repeat("x", (8<<20)+1) }, 65536, "size_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, text := spreadsheetAttachmentText(t.Context(), spreadsheetFixture(t, tc.change), tc.limit)
			if status != tc.expected || text != "" {
				t.Fatalf("got %s with %d text bytes", status, len(text))
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	status, text := spreadsheetAttachmentText(ctx, spreadsheetFixture(t, nil), 65536)
	if status == "supplied" || text != "" {
		t.Fatal("canceled extraction supplied content")
	}
}
