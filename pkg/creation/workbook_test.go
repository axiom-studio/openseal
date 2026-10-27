package creation

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestWorkbooksHoldLiteralTypedValues(t *testing.T) {
	{
		data, err := RenderWorkbook(t.Context(), []Sheet{{Name: "Budget", Header: true, Rows: [][]interface{}{{"Résumé", "Amount"}, {"=1+1", 12.5, true, nil}}}})
		if err != nil {
			t.Fatal(err)
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
