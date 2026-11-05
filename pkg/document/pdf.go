// Package document provides portable deterministic document capabilities.
// Hosts may replace Renderer with a richer implementation while preserving the
// same governed Skill contract and Artifact semantics.
package document

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID      = "openseal.document"
	SkillVersion = "1.0.2"
	SkillImage   = "axiomstudio/skill-openseal-document:1.0.2"
	RenderPDF    = "render_pdf"
)

type Report struct {
	Title string
	Body  string
}

type RenderedDocument struct {
	MediaType string
	Bytes     []byte
	Pages     int
}

type Renderer interface {
	RenderPDF(Report) (*RenderedDocument, error)
}

type PlainTextPDFRenderer struct{}

func (PlainTextPDFRenderer) RenderPDF(report Report) (*RenderedDocument, error) {
	title := strings.TrimSpace(report.Title)
	body := strings.TrimSpace(report.Body)
	if title == "" || body == "" {
		return nil, errors.New("PDF title and body are required")
	}
	if !asciiPrintable(title) || !asciiPrintable(body) {
		return nil, errors.New("plain-text PDF renderer supports printable ASCII; configure a Unicode renderer for this document")
	}
	lines := wrapLines(append([]string{title, ""}, strings.Split(body, "\n")...), 92)
	const linesPerPage = 52
	pages := make([][]string, 0, (len(lines)+linesPerPage-1)/linesPerPage)
	for len(lines) > 0 {
		count := min(linesPerPage, len(lines))
		pages = append(pages, append([]string(nil), lines[:count]...))
		lines = lines[count:]
	}
	pdf := encodePDF(pages)
	return &RenderedDocument{MediaType: "application/pdf", Bytes: pdf, Pages: len(pages)}, nil
}

func SkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Document renderer",
		Description: "Render durable reports into governed PDF artifacts.",
		Icon:        "file-text", Category: "documents", Tags: []string{"pdf", "report", "artifact"},
		Transport: skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{RenderPDF: {
			Name: RenderPDF, Description: "Render a title and report body as a PDF artifact.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"title", "body", "filename", "requirementName"},
				"properties": map[string]interface{}{
					"title":           map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 300},
					"body":            map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 500000},
					"filename":        map[string]interface{}{"type": "string", "pattern": `^[^/\\]{1,240}\.pdf$`},
					"requirementName": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128},
					"sources":         map[string]interface{}{"type": "array", "maxItems": 100, "items": map[string]interface{}{"type": "string", "format": "uri"}},
				},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"artifactRefs", "pages"},
				"properties": map[string]interface{}{
					"artifactRefs": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 1, "items": map[string]interface{}{
						"type": "object", "additionalProperties": false, "required": []interface{}{"id", "version", "requirementName"},
						"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string"}, "version": map[string]interface{}{"type": "integer", "minimum": 1}, "requirementName": map[string]interface{}{"type": "string"}},
					}},
					"pages": map[string]interface{}{"type": "integer", "minimum": 1},
				},
			},
			SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
			Retry: skill.ActionRetryPolicy{MaxAttempts: 1}, EmittedArtifactTypes: []string{"report"},
		}},
		Prompt:       &capability.PromptModule{Instructions: "Use render_pdf when work requires a durable PDF deliverable. Cite sources in the body before rendering.", UserInvocable: true, AllowedTools: []string{RenderPDF}},
		Requirements: capability.Requirements{AlwaysAvailable: true},
		Installers: []capability.Installer{{
			ID: "oci", Kind: "oci", Label: "Document renderer service image", Package: SkillImage,
		}},
	}
}

func asciiPrintable(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func wrapLines(lines []string, width int) []string {
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		for len(line) > width {
			cut := strings.LastIndex(line[:width+1], " ")
			if cut < width/2 {
				cut = width
			}
			result = append(result, strings.TrimSpace(line[:cut]))
			line = strings.TrimSpace(line[cut:])
		}
		result = append(result, line)
	}
	return result
}

func encodePDF(pages [][]string) []byte {
	objects := make([][]byte, 3+len(pages)*2)
	objects[0] = []byte("<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = strconv.Itoa(5+i*2) + " 0 R"
	}
	objects[1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objects[2] = []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for index, lines := range pages {
		var stream strings.Builder
		stream.WriteString("BT /F1 10 Tf 50 790 Td 14 TL\n")
		for _, line := range lines {
			stream.WriteString("(")
			stream.WriteString(escapePDFText(line))
			stream.WriteString(") Tj T*\n")
		}
		stream.WriteString("ET")
		content := stream.String()
		objects[3+index*2] = []byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
		objects[4+index*2] = []byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 842] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 4+index*2))
	}
	var output bytes.Buffer
	output.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n", index+1)
		output.Write(object)
		output.WriteString("\nendobj\n")
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}

func escapePDFText(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `(`, `\(`)
	return strings.ReplaceAll(value, `)`, `\)`)
}
