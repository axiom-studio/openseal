package conversationrun

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/google/uuid"
)

// Files and images an answer shows are markdown links to files saved in its
// chat (presentation.AnswerLinks). Delivery stores each linked file as it is
// now, attaches it to the message and rewrites the link to the stored
// file's content URL. Nothing the answer does not link is attached.

// LinkedFile is the stored version of a file an answer links.
type LinkedFile struct {
	ID        string
	Version   int64
	MediaType string
}

// ErrLinkedFileNotFound is what a ChatFileSnapshotter returns for a target
// that names no file of the Run's chat.
var ErrLinkedFileNotFound = errors.New("no such file in this chat")

// ChatFileSnapshotter is the host's chat file store, seen from delivery.
type ChatFileSnapshotter interface {
	// SnapshotChatFile stores the file target names in run's chat as it is
	// now and returns that stored version; an unchanged file returns the
	// version stored before. target is a chat-file path ("images/cat.png",
	// cleaned of "./" and a leading "/") or a saved file id. A target that
	// names no file of the chat returns an error wrapping
	// ErrLinkedFileNotFound.
	SnapshotChatFile(ctx context.Context, run *models.AgentRun, target string) (LinkedFile, error)
}

const (
	answerLinkRejectedEvent   = "turn.file_link_rejected"
	answerLinkRejectedSummary = "The agent's answer linked a file that is not in this chat:"
	// fileLinkRepairLead starts the request to fix links to files that do
	// not exist; a Run asks it once.
	fileLinkRepairLead = "Your answer links files that are not saved in this chat:"
)

func isFileLinkRepairRequest(text string) bool {
	return strings.HasPrefix(text, fileLinkRepairLead)
}

// fileLinkRepairRequest asks for the answer again with links to saved files.
func fileLinkRepairRequest(unknown []string, saved []models.SavedArtifact) string {
	var b strings.Builder
	b.WriteString(fileLinkRepairLead + " " + strings.Join(unknown, ", ") + ".")
	var paths []string
	for _, file := range saved {
		if file.Path != "" {
			paths = append(paths, file.Path)
		}
	}
	if len(paths) > 0 {
		b.WriteString(" Files this run saved: " + strings.Join(paths, ", ") + ".")
	}
	b.WriteString(" Link a file by its path in this chat's files, after saving it there, and send your whole answer again.")
	return b.String()
}

// answerFiles is an answer whose file links are resolved: content has every
// resolved link pointing at its stored file and every unknown one replaced
// by its text; references are the stored files in answer order.
type answerFiles struct {
	content    string
	references []models.ConversationReference
	unknown    []string
}

// resolveAnswerFiles resolves the file links of content: first against the
// files this Run's tools saved (by id or path, newest version), then through
// the host's chat file store. Links with a URL scheme are left alone.
func (r *TurnRunner) resolveAnswerFiles(ctx context.Context, run *models.AgentRun, content string, saved []models.SavedArtifact) (answerFiles, error) {
	resolved := map[string]*LinkedFile{}
	var out answerFiles
	attached := map[string]bool{}
	for _, link := range presentation.AnswerLinks(content) {
		if !presentation.FileTarget(link.Target) {
			continue
		}
		target := linkFilePath(link.Target)
		if _, done := resolved[target]; done {
			continue
		}
		file, err := r.linkedFile(ctx, run, target, saved)
		if err != nil {
			return answerFiles{}, err
		}
		resolved[target] = file
		if file == nil {
			out.unknown = append(out.unknown, target)
			continue
		}
		key := fmt.Sprintf("%s\x00%d", file.ID, file.Version)
		if !attached[key] {
			attached[key] = true
			out.references = append(out.references, models.ConversationReference{Kind: models.ConversationReferenceArtifact, ID: file.ID, Version: file.Version})
		}
	}
	out.content = presentation.RewriteLinks(content, func(link presentation.Link) string {
		original := content[link.Start:link.End]
		if !presentation.FileTarget(link.Target) {
			return original
		}
		file := resolved[linkFilePath(link.Target)]
		if file == nil {
			return link.Label
		}
		if link.Image && file.MediaType != "" && !strings.HasPrefix(file.MediaType, "image/") {
			// Only images show inline; any other file is a file link.
			link.Image = false
		}
		return presentation.LinkTo(link, presentation.ArtifactContentURL(file.ID, file.Version))
	})
	return out, nil
}

// linkedFile is the stored file target names, or nil when there is none.
func (r *TurnRunner) linkedFile(ctx context.Context, run *models.AgentRun, target string, saved []models.SavedArtifact) (*LinkedFile, error) {
	var newest *models.SavedArtifact
	for i, file := range saved {
		if (file.Ref.ID == target || file.Path != "" && file.Path == target) && (newest == nil || file.Ref.Version > newest.Ref.Version) {
			newest = &saved[i]
		}
	}
	if newest != nil {
		return &LinkedFile{ID: newest.Ref.ID, Version: newest.Ref.Version}, nil
	}
	if r.config.ChatFiles == nil {
		return nil, nil
	}
	file, err := r.config.ChatFiles.SnapshotChatFile(ctx, run, target)
	if errors.Is(err, ErrLinkedFileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store linked file %q: %w", target, err)
	}
	if file.ID == "" || file.Version < 1 {
		return nil, fmt.Errorf("store linked file %q: the chat file store returned no stored version", target)
	}
	return &file, nil
}

// linkFilePath is a link target as a chat path: "./docs/a.pdf",
// "/docs/a.pdf" and "docs/a%20b.pdf" name "docs/a.pdf" and "docs/a b.pdf".
func linkFilePath(target string) string {
	target = strings.TrimSpace(target)
	if unescaped, err := url.PathUnescape(target); err == nil {
		target = unescaped
	}
	for strings.HasPrefix(target, "./") {
		target = target[2:]
	}
	return strings.TrimLeft(target, "/")
}

// fileLinkRejected records a link to a file that is not in the chat, left
// as its text on the posted answer. A lost record is not retried.
func (h *conversationFinalHandler) fileLinkRejected(ctx context.Context, run *models.AgentRun, step *models.AgentTurn, workerID, target string) {
	if h.activity == nil {
		return
	}
	stepID := ""
	if step != nil {
		stepID = step.ID
	}
	event := &models.ActivityEvent{
		ID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join([]string{"file-link-rejected", run.ID, stepID, target}, "\x00"))).String(),
		Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: models.TeamIDForRun(run),
		EventType: answerLinkRejectedEvent, Severity: models.ActivitySeverityWarning, CreatedAt: h.runner.now(),
		Summary: answerLinkRejectedSummary + " " + target, Actor: models.ActivityActor{Type: "worker", ID: workerID},
		Payload:    map[string]interface{}{"target": target},
		Visibility: models.ActivityVisibilityScope,
	}
	_, _ = h.activity.AppendActivity(ctx, event)
}
