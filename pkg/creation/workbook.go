package creation

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Sheet struct {
	Name   string          `json:"name"`
	Rows   [][]interface{} `json:"rows"`
	Header bool            `json:"header"`
}

const ns = `http://schemas.openxmlformats.org/spreadsheetml/2006/main`

func escape(s string) string { var b bytes.Buffer; xml.EscapeText(&b, []byte(s)); return b.String() }
func column(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+n%26)) + s
		n /= 26
	}
	return s
}
func xmlText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != 9 && r != 10 && r != 13 && (r < 32 || r == 0xfffe || r == 0xffff) {
			return false
		}
	}
	return true
}
func RenderWorkbook(ctx context.Context, sheets []Sheet) ([]byte, error) {
	if len(sheets) < 1 || len(sheets) > 20 {
		return nil, errors.New("a workbook requires 1 to 20 sheets")
	}
	names := map[string]bool{}
	cells := 0
	var out bytes.Buffer
	archive := zip.NewWriter(&out)
	write := func(name, source string) error {
		w, err := archive.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(xml.Header + source))
		return err
	}
	types := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`
	book := `<workbook xmlns="` + ns + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`
	rels := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="styles" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`
	for i, sheet := range sheets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := strings.ToLower(sheet.Name)
		if !textBound(sheet.Name, 31) || !xmlText(sheet.Name) || strings.ContainsAny(sheet.Name, `[]:*?/\`) || strings.HasPrefix(sheet.Name, "'") || strings.HasSuffix(sheet.Name, "'") || names[key] {
			return nil, errors.New("sheet names must be valid and unique")
		}
		names[key] = true
		if len(sheet.Rows) < 1 || len(sheet.Rows) > 10000 {
			return nil, errors.New("each sheet requires 1 to 10000 rows")
		}
		maxCols := 1
		for _, row := range sheet.Rows {
			if len(row) > 256 {
				return nil, errors.New("a row exceeds 256 columns")
			}
			if len(row) > maxCols {
				maxCols = len(row)
			}
			cells += len(row)
		}
		if cells > 100000 {
			return nil, errors.New("workbook exceeds 100000 cells")
		}
		dimension := fmt.Sprintf("A1:%s%d", column(maxCols), len(sheet.Rows))
		var body strings.Builder
		fmt.Fprintf(&body, `<worksheet xmlns="%s"><dimension ref="%s"/><sheetViews><sheetView workbookViewId="0">`, ns, dimension)
		if sheet.Header {
			body.WriteString(`<pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/>`)
		}
		body.WriteString(`</sheetView></sheetViews><sheetFormatPr defaultRowHeight="18"/><cols>`)
		fmt.Fprintf(&body, `<col min="1" max="%d" width="24" customWidth="1"/></cols><sheetData>`, maxCols)
		for r, row := range sheet.Rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fmt.Fprintf(&body, `<row r="%d">`, r+1)
			for c, value := range row {
				if value == nil {
					continue
				}
				ref := column(c+1) + strconv.Itoa(r+1)
				style := ""
				if sheet.Header && r == 0 {
					style = ` s="1"`
				}
				switch v := value.(type) {
				case string:
					if utf8.RuneCountInString(v) > 32767 || !xmlText(v) {
						return nil, errors.New("invalid or oversized cell text")
					}
					fmt.Fprintf(&body, `<c r="%s"%s t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, style, escape(v))
				case float64:
					if math.IsInf(v, 0) || math.IsNaN(v) {
						return nil, errors.New("cell number is not finite")
					}
					fmt.Fprintf(&body, `<c r="%s"%s><v>%s</v></c>`, ref, style, strconv.FormatFloat(v, 'g', -1, 64))
				case bool:
					number := 0
					if v {
						number = 1
					}
					fmt.Fprintf(&body, `<c r="%s"%s t="b"><v>%d</v></c>`, ref, style, number)
				default:
					return nil, errors.New("cells must contain text, numbers, booleans, or null")
				}
			}
			body.WriteString(`</row>`)
		}
		body.WriteString(`</sheetData>`)
		if sheet.Header {
			fmt.Fprintf(&body, `<autoFilter ref="%s"/>`, dimension)
		}
		body.WriteString(`</worksheet>`)
		filename := fmt.Sprintf("sheet%d.xml", i+1)
		if err := write("xl/worksheets/"+filename, body.String()); err != nil {
			return nil, err
		}
		types += fmt.Sprintf(`<Override PartName="/xl/worksheets/%s" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, filename)
		book += fmt.Sprintf(`<sheet name="%s" sheetId="%d" r:id="sheet%d"/>`, escape(sheet.Name), i+1, i+1)
		rels += fmt.Sprintf(`<Relationship Id="sheet%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/%s"/>`, i+1, filename)
	}
	styles := `<styleSheet xmlns="` + ns + `"><fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts><fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFDDEDE6"/><bgColor indexed="64"/></patternFill></fill></fills><borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`
	files := [][2]string{{"[Content_Types].xml", types + `</Types>`}, {"_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="workbook" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`}, {"xl/workbook.xml", book + `</sheets></workbook>`}, {"xl/_rels/workbook.xml.rels", rels + `</Relationships>`}, {"xl/styles.xml", styles}}
	for _, file := range files {
		if err := write(file[0], file[1]); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
