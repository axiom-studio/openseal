package presentation

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Files and images an answer shows are markdown links to files saved in its
// chat: ![alt](images/cat.png) or [label](docs/report.pdf). Delivery stores
// each linked file and rewrites the target to the file's artifact content
// URL, the form posted messages keep.

// Link is one inline markdown link or image of an answer, outside code.
type Link struct {
	Image  bool
	Label  string // the alt text of an image, the text of a link
	Target string // as written, without <> and title
	Start  int    // byte offset of the link ("!" included for an image)
	End    int
}

// ArtifactContentPath starts the URL of a stored file's content.
const ArtifactContentPath = "/orchestrator/agent/kernel/v1/artifacts/"

// ArtifactContentURL is the URL a posted message links a stored file by.
func ArtifactContentURL(id string, version int64) string {
	return fmt.Sprintf("%s%s/content?version=%d", ArtifactContentPath, url.PathEscape(id), version)
}

// ArtifactContentRef reads an ArtifactContentURL back; ok is false for any
// other target.
func ArtifactContentRef(target string) (id string, version int64, ok bool) {
	rest, found := strings.CutPrefix(target, ArtifactContentPath)
	if !found {
		return "", 0, false
	}
	escaped, query, _ := strings.Cut(rest, "?")
	escaped, found = strings.CutSuffix(escaped, "/content")
	if !found || escaped == "" || strings.Contains(escaped, "/") {
		return "", 0, false
	}
	id, err := url.PathUnescape(escaped)
	if err != nil {
		return "", 0, false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", 0, false
	}
	version, err = strconv.ParseInt(values.Get("version"), 10, 64)
	if err != nil || version < 1 {
		return "", 0, false
	}
	return id, version, true
}

// FileTarget reports a link target that names a file of the chat: neither
// a URL with a scheme (https:, mailto:, data:), nor an anchor, nor a
// stored file's content URL.
func FileTarget(target string) bool {
	target = strings.TrimSpace(target)
	if target == "" || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "//") {
		return false
	}
	if _, _, ok := ArtifactContentRef(target); ok {
		return false
	}
	if colon := strings.IndexByte(target, ':'); colon > 0 && !strings.ContainsAny(target[:colon], "/.") {
		return false
	}
	return true
}

// AnswerLinks lists the inline links and images of answer in order. Links
// inside code fences and code spans are text, not links.
func AnswerLinks(answer string) []Link {
	var links []Link
	fence := ""
	offset := 0
	for offset < len(answer) {
		end := len(answer)
		if next := strings.IndexByte(answer[offset:], '\n'); next >= 0 {
			end = offset + next + 1
		}
		line := answer[offset:end]
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			if strings.HasPrefix(trimmed, fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
		case codeFence.MatchString(strings.TrimRight(line, "\r\n")):
			fence = codeFence.FindStringSubmatch(strings.TrimRight(line, "\r\n"))[1]
		default:
			for _, link := range lineLinks(line) {
				link.Start += offset
				link.End += offset
				links = append(links, link)
			}
		}
		offset = end
	}
	return links
}

// lineLinks reads the inline links of one line, skipping code spans.
func lineLinks(line string) []Link {
	var links []Link
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '`':
			run := 1
			for i+run < len(line) && line[i+run] == '`' {
				run++
			}
			if close := strings.Index(line[i+run:], strings.Repeat("`", run)); close >= 0 {
				i += run + close + run - 1
			} else {
				i += run - 1
			}
		case '[':
			link, ok := readLink(line, i)
			if !ok {
				continue
			}
			if i > 0 && line[i-1] == '!' && (i < 2 || line[i-2] != '\\') {
				link.Image = true
				link.Start--
			}
			links = append(links, link)
			i = link.End - 1
		}
	}
	return links
}

// readLink reads [label](target "title") at line[start] == '['.
func readLink(line string, start int) (Link, bool) {
	depth := 0
	closeLabel := -1
	for i := start; i < len(line) && closeLabel < 0; i++ {
		switch line[i] {
		case '\\':
			i++
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				closeLabel = i
			}
		}
	}
	if closeLabel < 0 || closeLabel+1 >= len(line) || line[closeLabel+1] != '(' {
		return Link{}, false
	}
	rest := line[closeLabel+2:]
	var target string
	var used int
	if strings.HasPrefix(rest, "<") {
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return Link{}, false
		}
		target, used = rest[1:end], end+1
	} else {
		depth := 0
		used = len(rest)
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if c == '(' {
				depth++
			} else if c == ')' && depth > 0 {
				depth--
			} else if c == ')' || c == ' ' || c == '\t' {
				used = i
				break
			}
		}
		target = rest[:used]
	}
	closeParen := strings.IndexByte(rest[used:], ')')
	if closeParen < 0 || strings.TrimSpace(target) == "" {
		return Link{}, false
	}
	if title := strings.TrimSpace(rest[used : used+closeParen]); title != "" && !quotedTitle(title) {
		return Link{}, false
	}
	return Link{Label: line[start+1 : closeLabel], Target: target, Start: start, End: closeLabel + 2 + used + closeParen + 1}, true
}

func quotedTitle(title string) bool {
	if len(title) < 2 {
		return false
	}
	switch title[0] {
	case '"', '\'':
		return title[len(title)-1] == title[0]
	case '(':
		return title[len(title)-1] == ')'
	}
	return false
}

// RewriteLinks replaces each link of answer with what replace returns for
// it; replace returns the link's own text to keep it.
func RewriteLinks(answer string, replace func(Link) string) string {
	var b strings.Builder
	last := 0
	for _, link := range AnswerLinks(answer) {
		b.WriteString(answer[last:link.Start])
		b.WriteString(replace(link))
		last = link.End
	}
	b.WriteString(answer[last:])
	return b.String()
}

// LinkTo is link written again with target, keeping its label and kind.
func LinkTo(link Link, target string) string {
	prefix := ""
	if link.Image {
		prefix = "!"
	}
	return prefix + "[" + link.Label + "](" + target + ")"
}

// plainLinks drops the stored files an answer shows: an image goes, a file
// link becomes its label. Other links stay.
func plainLinks(answer string) string {
	return RewriteLinks(answer, func(link Link) string {
		if _, _, ok := ArtifactContentRef(link.Target); !ok {
			return answer[link.Start:link.End]
		}
		if link.Image {
			return ""
		}
		return link.Label
	})
}
