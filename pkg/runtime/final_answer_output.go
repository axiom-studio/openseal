package runtime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

// finalOutputFilesKey lists the ids of the files saved in this Run that
// the answer hands over; only those go out with it.
const finalOutputFilesKey = "files"

// cloneVisual deep-copies a visual so forms, responses and outcomes never
// share component slices.
func cloneVisual(visual *presentation.Visual) *presentation.Visual {
	if visual == nil {
		return nil
	}
	encoded, err := json.Marshal(visual)
	if err != nil {
		copy := *visual
		return &copy
	}
	var cloned presentation.Visual
	if json.Unmarshal(encoded, &cloned) != nil {
		copy := *visual
		return &copy
	}
	return &cloned
}

// handedOverFileIDs trims and deduplicates the answer's file ids.
func handedOverFileIDs(ids []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// handedOverArtifacts keeps the Run's saved files the final output hands over
// (its "files", by id or by the path a tool saved it from) and the ones the
// Agent showed in the chat itself (save_app, present), in the Run's order.
// A file saved more than once goes out as its newest version only. A Run's
// other files stay in the Library. unknown lists the entries that name no
// saved file.
func handedOverArtifacts(saved []savedArtifact, output map[string]interface{}) (out []ConversationReference, unknown []string) {
	var wanted []string
	switch files := output[finalOutputFilesKey].(type) {
	case []string:
		wanted = files
	case []interface{}:
		for _, id := range files {
			if text, ok := id.(string); ok {
				wanted = append(wanted, text)
			}
		}
	}
	chosen := map[string]bool{}
	for _, file := range saved {
		if file.Shown {
			chosen[file.Ref.ID] = true
		}
	}
	for _, name := range handedOverFileIDs(wanted) {
		found := false
		for _, file := range saved {
			if file.Ref.ID == name || file.Path != "" && file.Path == handedOverPath(name) {
				chosen[file.Ref.ID], found = true, true
			}
		}
		if !found {
			unknown = append(unknown, name)
		}
	}
	newest := map[string]int{}
	for i, file := range saved {
		if at, ok := newest[file.Ref.ID]; !ok || file.Ref.Version >= saved[at].Ref.Version {
			newest[file.Ref.ID] = i
		}
	}
	for i, file := range saved {
		if chosen[file.Ref.ID] && newest[file.Ref.ID] == i {
			out = append(out, file.Ref)
		}
	}
	return out, unknown
}

// handedOverPath is a chat path as an answer may write it: "docs/a.pdf",
// "/docs/a.pdf" or "./docs/a.pdf" all name the same file.
func handedOverPath(name string) string {
	name = strings.TrimSpace(name)
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	return strings.TrimLeft(name, "/")
}

// unknownHandedOverFilesText sends a final whose files name no saved file
// back to the model, which otherwise attaches nothing without a word.
func unknownHandedOverFilesText(unknown []string, saved []savedArtifact) string {
	names := make([]string, 0, len(saved))
	for _, file := range saved {
		if file.Path != "" {
			names = append(names, file.Path)
		} else {
			names = append(names, file.Ref.ID)
		}
	}
	if len(names) == 0 {
		return fmt.Sprintf("files names %s, but this run handed over no files yet. present the file first, then name it in files, or leave files out.", strings.Join(unknown, ", "))
	}
	return fmt.Sprintf("files names %s, which this run did not present. Present it first, or name only files this run presented: %s.",
		strings.Join(unknown, ", "), strings.Join(names, ", "))
}
