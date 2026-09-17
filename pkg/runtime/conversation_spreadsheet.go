package runtime

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

const spreadsheetMediaType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

type spreadsheetString struct {
	Text string `xml:"t"`
	Runs []struct {
		Text string `xml:"t"`
	} `xml:"r"`
}

func (s spreadsheetString) value() string {
	var value strings.Builder
	value.WriteString(s.Text)
	for _, run := range s.Runs {
		value.WriteString(run.Text)
	}
	return value.String()
}

// Read only workbook XML and cell data from a bounded ZIP. No extraction to disk,
// formula evaluation, URL fetching, macros, styles, or executable relationships.
func spreadsheetAttachmentText(ctx context.Context, content []byte, limit int) (string, string) {
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "invalid_document", ""
	}
	if len(archive.File) > 1024 {
		return "size_limit", ""
	}
	files := make(map[string]*zip.File)
	var expanded uint64
	for _, file := range archive.File {
		if file.UncompressedSize64 > 8<<20 {
			return "size_limit", ""
		}
		expanded += file.UncompressedSize64
		if expanded > 8<<20 {
			return "size_limit", ""
		}
		if _, duplicate := files[file.Name]; duplicate {
			return "invalid_document", ""
		}
		files[file.Name] = file
	}
	readXML := func(name string, target interface{}) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file := files[name]
		if file == nil {
			return fmt.Errorf("missing workbook part")
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, 8<<20+1))
		if err != nil || len(data) > 8<<20 {
			return fmt.Errorf("invalid workbook part")
		}
		return xml.Unmarshal(data, target)
	}
	var workbook struct {
		Sheets []struct {
			Name string `xml:"name,attr"`
			ID   string `xml:"id,attr"`
		} `xml:"sheets>sheet"`
	}
	var relations struct {
		Items []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
			Type   string `xml:"Type,attr"`
		} `xml:"Relationship"`
	}
	if readXML("xl/workbook.xml", &workbook) != nil || readXML("xl/_rels/workbook.xml.rels", &relations) != nil {
		return "invalid_document", ""
	}
	if len(workbook.Sheets) == 0 || len(workbook.Sheets) > 64 {
		return "unsupported_document", ""
	}
	var shared struct {
		Items []spreadsheetString `xml:"si"`
	}
	if files["xl/sharedStrings.xml"] != nil && readXML("xl/sharedStrings.xml", &shared) != nil {
		return "invalid_document", ""
	}
	var output strings.Builder
	output.WriteString("Spreadsheet cell values. Formulas are not evaluated; cached values may be stale. Numeric values are raw and may represent dates; formatting is not reproduced.\n")
	for _, sheet := range workbook.Sheets {
		if ctx.Err() != nil {
			return "unavailable", ""
		}
		part := ""
		for _, relation := range relations.Items {
			if relation.ID != sheet.ID || relation.Mode == "External" || !strings.HasSuffix(relation.Type, "/worksheet") {
				continue
			}
			part = path.Clean(path.Join("xl", relation.Target))
			if strings.HasPrefix(relation.Target, "/") {
				part = strings.TrimPrefix(path.Clean(relation.Target), "/")
			}
			if !strings.HasPrefix(part, "xl/worksheets/") {
				return "invalid_document", ""
			}
		}
		var worksheet struct {
			Rows []struct {
				Cells []struct {
					Ref     string            `xml:"r,attr"`
					Type    string            `xml:"t,attr"`
					Value   string            `xml:"v"`
					Formula string            `xml:"f"`
					Inline  spreadsheetString `xml:"is"`
				} `xml:"c"`
			} `xml:"sheetData>row"`
		}
		if part == "" || readXML(part, &worksheet) != nil {
			return "invalid_document", ""
		}
		fmt.Fprintf(&output, "Sheet %q\n", sheet.Name)
		for _, row := range worksheet.Rows {
			for _, cell := range row.Cells {
				value := cell.Value
				switch cell.Type {
				case "s":
					index, err := strconv.Atoi(value)
					if err != nil || index < 0 || index >= len(shared.Items) {
						return "invalid_document", ""
					}
					value = shared.Items[index].value()
				case "inlineStr":
					value = cell.Inline.value()
				case "b":
					if value == "1" {
						value = "true"
					} else if value == "0" {
						value = "false"
					}
				}
				if value == "" && cell.Formula != "" {
					value = "[formula has no cached value]"
				}
				if value == "" {
					continue
				}
				fmt.Fprintf(&output, "%s: %q\n", cell.Ref, value)
				if output.Len() > limit {
					return "size_limit", ""
				}
			}
		}
	}
	if output.Len() > limit {
		return "size_limit", ""
	}
	return "supplied", output.String()
}
