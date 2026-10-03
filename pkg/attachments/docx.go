package attachments

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"
)

// DOCXText reads only the document body. It never resolves relationships,
// embedded objects, macros or external resources, and bounds decompression.
func DOCXText(ctx context.Context, content []byte) (string, string) {
	if len(content) > MaximumFileBytes {
		return "size_limit", ""
	}
	archive, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return "unreadable", ""
	}
	for _, file := range archive.File {
		if file.Name != "word/document.xml" {
			continue
		}
		const maximumXMLBytes = 4 << 20
		if file.UncompressedSize64 > maximumXMLBytes {
			return "size_limit", ""
		}
		reader, err := file.Open()
		if err != nil {
			return "unreadable", ""
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, maximumXMLBytes+1))
		if err != nil {
			return "unreadable", ""
		}
		if len(data) > maximumXMLBytes {
			return "size_limit", ""
		}
		decoder := xml.NewDecoder(bytes.NewReader(data))
		var result strings.Builder
		inText := false
		for {
			if ctx.Err() != nil {
				return "unreadable", ""
			}
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "unreadable", ""
			}
			switch value := token.(type) {
			case xml.StartElement:
				if value.Name.Local == "t" {
					inText = true
				}
				if value.Name.Local == "tab" {
					result.WriteByte('\t')
				}
				if value.Name.Local == "br" {
					result.WriteByte('\n')
				}
			case xml.EndElement:
				if value.Name.Local == "t" {
					inText = false
				}
				if value.Name.Local == "p" {
					result.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					result.Write(value)
				}
			}
			if result.Len() > MaximumTextBytes {
				return "size_limit", ""
			}
		}
		text := strings.TrimSpace(result.String())
		if text == "" {
			return "no_extractable_text", ""
		}
		return "supplied", text
	}
	return "unreadable", ""
}
