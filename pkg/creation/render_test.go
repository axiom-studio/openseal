package creation

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestHostRenderTypedWorkbookAndCompletePage(t *testing.T) {
	for _, kind := range []string{Page, Workbook} {
		config := map[string]interface{}{"artifactId": "brief", "expectedLatestVersion": 0, "title": "Brief", "requirementName": "brief"}
		if kind == Page {
			config["filename"] = "brief.html"
			config["html"] = "<!doctype html><html><body>Résumé &amp; notes</body></html>"
		} else {
			config["filename"] = "brief.xlsx"
			config["sheets"] = []interface{}{map[string]interface{}{"name": "Budget", "header": true, "rows": []interface{}{[]interface{}{"Résumé", "Amount"}, []interface{}{"=1+1", 12.5, true, nil}}}}
		}
		output, err := Render(t.Context(), kind, config)
		if err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(output["_transientArtifactBase64"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if kind == Page {
			if string(data) != config["html"] {
				t.Fatal("page source changed")
			}
			config["html"] = "fragment"
			if _, err := Render(t.Context(), kind, config); err == nil {
				t.Fatal("fragment accepted")
			}
			continue
		}
		z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range z.File {
			reader, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			decoder := xml.NewDecoder(bytes.NewReader(raw))
			for {
				_, err := decoder.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("invalid XML %s: %v", f.Name, err)
				}
			}
			if f.Name == "xl/worksheets/sheet1.xml" {
				found = true
				for _, want := range []string{"=1+1", "12.5", "inlineStr", "frozen", "autoFilter"} {
					if !strings.Contains(string(raw), want) {
						t.Fatalf("missing %s", want)
					}
				}
				if strings.Contains(string(raw), "<f>") {
					t.Fatal("literal became a formula")
				}
			}
		}
		if !found {
			t.Fatal("worksheet missing")
		}
	}
}

func TestHostWorkbookRejectsInvalidSheetsAndValues(t *testing.T) {
	for _, sheets := range [][]Sheet{
		{{Name: "Bad/name", Rows: [][]interface{}{{"value"}}}},
		{{Name: "Budget", Rows: [][]interface{}{{"value"}}}, {Name: "budget", Rows: [][]interface{}{{"value"}}}},
		{{Name: "Budget", Rows: [][]interface{}{{map[string]interface{}{"unsupported": true}}}}},
		{{Name: "Budget", Rows: [][]interface{}{{"bad\x00text"}}}},
	} {
		if _, err := RenderWorkbook(t.Context(), sheets); err == nil {
			t.Fatal("invalid workbook accepted")
		}
	}
}
