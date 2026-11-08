package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strconv"
	"strings"
)

// Session path rendering (SPEC §3.6): projectSessionPath turns the stored
// path into ModelStepRequest.Entries.
//
//   - R1: a tool entry renders right after its assistant entry, in the order
//     of the assistant's tool calls. Entries appended between them (another
//     Run of the lane, a steering message) render after the group.
//   - R2: a tool entry that is not terminal renders as completed with
//     renderOpenToolText.
//   - R3: a tool entry with no matching assistant entry on the path is
//     dropped. An assistant tool call with no tool entry on the path (a
//     sibling call cut off by a fork or a promotion) gets a synthetic
//     completed result, renderMissingToolText, so every call is answered.
//   - Images resolve to DataBase64 from the artifact store or the governed
//     call's output.modelMedia. Without image input every image becomes
//     "[image omitted: <mediaType>]"; an image whose bytes cannot be read or
//     do not match becomes "[image unavailable: <mediaType>]".
//   - Compactions render CompactionSummaryText.
//
// View sequences are render positions, not stored sequences: a forked path
// restarts sequences in every child session and R1 moves tool entries ahead
// of interleaved entries, while the model step contract wants one ascending
// order. With a compaction, the kept tail is numbered first, then the
// compaction, then the later entries, so "before the compaction" keeps its
// meaning for the step host's reasoning rules.

const (
	renderOpenToolText    = "This call has not finished yet; its result is not available in this conversation."
	renderMissingToolText = "This call's result is not part of this conversation."
)

// fitToolResultView keeps a rendered tool result within the tool result cap.
// Results are bounded when they are written, so this only shortens one that
// was stored before the cap or measured differently; a step must never fail
// because an earlier result is a little too large.
func fitToolResultView(content []ContentPart) []ContentPart {
	var textual, others []ContentPart
	total := 0
	for _, part := range content {
		switch part.Type {
		case ContentPartText:
			textual = append(textual, part)
			total += len(part.Text)
		case ContentPartJSON:
			textual = append(textual, part)
			total += jsonPartBytes(part.Value)
		default:
			others = append(others, part)
		}
	}
	if total <= MaxToolResultContentBytes {
		return content
	}
	return append(compactToolResultContent(textual, total, "it was too large to show in full"), others...)
}

func renderImageOmittedText(mediaType string) string {
	return "[image omitted: " + mediaType + "]"
}

func renderImageUnavailableText(mediaType string) string {
	return "[image unavailable: " + mediaType + "]"
}

// renderActionCalls reads governed calls for their screenshots.
type renderActionCalls interface {
	GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error)
}

// sessionRenderOptions are the sources and binding facts of one render.
type sessionRenderOptions struct {
	// ImageInput is true when the bound model accepts image input (the
	// catalog modality the binding resolved). When false no image bytes are
	// read.
	ImageInput bool
	Artifacts  ArtifactStore
	Content    ArtifactContentStore
	Actions    renderActionCalls
}

// projectSessionPath renders path in model step order. A store error other
// than a missing artifact or call is returned; a missing or mismatched image
// renders as text.
func projectSessionPath(ctx context.Context, path *AgentSessionPath, opts sessionRenderOptions) ([]SessionEntryView, error) {
	if path == nil {
		return nil, nil
	}
	r := &sessionRenderer{opts: opts, images: map[string]*ContentPart{}}
	if path.Session != nil {
		r.scope = path.Session.Scope
	}

	// Tool entries by owning step and call id (R1/R3). Directive entries have
	// no step and no assistant, so they never render.
	tools := make(map[string]*SessionEntry)
	for _, entry := range path.Entries {
		if entry == nil || entry.Type != SessionEntryTool || entry.StepID == "" || entry.ToolCallID == "" {
			continue
		}
		key := entry.StepID + "\x00" + entry.ToolCallID
		if _, seen := tools[key]; !seen {
			tools[key] = entry
		}
	}

	segment := renderSegmentIndex(path)
	var compaction *CompactionEntry
	if path.Compaction != nil {
		compaction = path.Compaction.Compaction
	}
	var kept, later []SessionEntryView
	for _, entry := range path.Entries {
		if entry == nil || entry.Type == SessionEntryTool || entry.Type == SessionEntryCompaction {
			continue
		}
		views, err := r.entry(ctx, entry, tools)
		if err != nil {
			return nil, err
		}
		if compaction != nil && renderBeforeCompaction(entry, path.Compaction, segment) {
			kept = append(kept, views...)
		} else {
			later = append(later, views...)
		}
	}

	out := make([]SessionEntryView, 0, len(kept)+len(later)+1)
	var sequence int64
	if compaction != nil {
		view := SessionEntryView{
			ID: path.Compaction.ID, Type: SessionEntryCompaction, CreatedAt: path.Compaction.CreatedAt,
			Compaction: &CompactionEntryView{Summary: CompactionSummaryText(compaction), TokensBefore: compaction.TokensBefore},
		}
		if len(kept) > 0 {
			view.Compaction.FirstKeptEntryID = kept[0].ID
		}
		out = append(out, view)
		for i := range kept {
			sequence++
			kept[i].Sequence = sequence
		}
		sequence++
		out[0].Sequence = sequence
		out = append(out, kept...)
	}
	for i := range later {
		sequence++
		later[i].Sequence = sequence
	}
	return append(out, later...), nil
}

// renderSegmentIndex maps each session of the path to its chain position.
func renderSegmentIndex(path *AgentSessionPath) map[string]int {
	index := make(map[string]int, len(path.Chain)+1)
	for i, session := range path.Chain {
		if session != nil {
			index[session.ID] = i
		}
	}
	if path.Session != nil {
		if _, ok := index[path.Session.ID]; !ok {
			index[path.Session.ID] = len(path.Chain)
		}
	}
	return index
}

// renderBeforeCompaction reports whether entry precedes the compaction on the
// path (the kept tail).
func renderBeforeCompaction(entry, compaction *SessionEntry, segment map[string]int) bool {
	if entry.SessionID == compaction.SessionID {
		return entry.Sequence < compaction.Sequence
	}
	at, ok := segment[entry.SessionID]
	cut, cutOK := segment[compaction.SessionID]
	return ok && cutOK && at < cut
}

type sessionRenderer struct {
	opts   sessionRenderOptions
	scope  Scope
	images map[string]*ContentPart // resolved image by source; nil value = unavailable
}

// entry renders one non-tool entry; an assistant entry brings its tool group.
func (r *sessionRenderer) entry(ctx context.Context, entry *SessionEntry, tools map[string]*SessionEntry) ([]SessionEntryView, error) {
	view := SessionEntryView{ID: entry.ID, Type: entry.Type, CreatedAt: entry.CreatedAt}
	switch entry.Type {
	case SessionEntryUser:
		if entry.User == nil {
			return nil, fmt.Errorf("%w: user entry %s has no payload", ErrInvalidSessionEntry, entry.ID)
		}
		content, err := r.parts(ctx, entry.User.Content)
		if err != nil {
			return nil, err
		}
		view.User = &UserEntryView{Author: renderUserAuthor(entry.User), Content: content}
	case SessionEntryNote:
		if entry.Note == nil {
			return nil, fmt.Errorf("%w: note entry %s has no payload", ErrInvalidSessionEntry, entry.ID)
		}
		view.Note = &NoteEntryView{Kind: string(entry.Note.Kind), Text: entry.Note.Text}
	case SessionEntryModelChange:
		if entry.ModelChange == nil {
			return nil, fmt.Errorf("%w: model_change entry %s has no payload", ErrInvalidSessionEntry, entry.ID)
		}
		change := *entry.ModelChange
		view.ModelChange = &change
	case SessionEntryAssistant:
		if entry.Assistant == nil {
			return nil, fmt.Errorf("%w: assistant entry %s has no payload", ErrInvalidSessionEntry, entry.ID)
		}
		assistant := *entry.Assistant
		assistant.Limits = nil
		assistant.ToolCalls = append([]ModelToolCall(nil), entry.Assistant.ToolCalls...)
		view.Assistant = &assistant
		views := []SessionEntryView{view}
		for _, call := range assistant.ToolCalls {
			tool, err := r.tool(ctx, entry, call, tools[entry.StepID+"\x00"+call.ID])
			if err != nil {
				return nil, err
			}
			views = append(views, tool)
		}
		return views, nil
	default:
		return nil, nil
	}
	return []SessionEntryView{view}, nil
}

// renderUserAuthor attributes intervention entries. Channel messages carry
// their attribution in the formatted header of the first text part, and goal
// briefs are kernel text, so both render without an author.
func renderUserAuthor(user *UserEntry) *EntryAuthor {
	if user.Origin != UserEntryIntervention {
		return nil
	}
	label := strings.TrimSpace(user.Author.DisplayName)
	if label == "" {
		label = strings.TrimSpace(user.Author.ID)
	}
	if label == "" {
		return nil
	}
	kind := EntryAuthorPerson
	if user.Author.Type == "agent" || user.Author.Type == "service" {
		kind = EntryAuthorAgent
	}
	return &EntryAuthor{Kind: kind, ID: user.Author.ID, Label: label}
}

// tool renders the result of one assistant tool call (R1-R3).
func (r *sessionRenderer) tool(ctx context.Context, assistant *SessionEntry, call ModelToolCall, entry *SessionEntry) (SessionEntryView, error) {
	arguments := call.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	if entry == nil || entry.Tool == nil {
		return SessionEntryView{
			ID:        SessionEntryID(assistant.SessionID, "render-missing:"+assistant.StepID+":"+call.ID),
			Type:      SessionEntryTool,
			CreatedAt: assistant.CreatedAt,
			Tool: &ToolEntryView{
				CallID: call.ID, Name: call.Name, Arguments: arguments, State: ToolEntryCompleted,
				Result: ToolResultView{Content: []ContentPart{{Type: ContentPartText, Text: renderMissingToolText}}},
			},
		}, nil
	}
	name := entry.Tool.Name
	if name == "" {
		name = call.Name
	}
	if len(entry.Tool.Arguments) > 0 {
		arguments = entry.Tool.Arguments
	}
	view := &ToolEntryView{CallID: call.ID, Name: name, Arguments: arguments, ActionCallID: entry.ActionCallID}
	switch {
	case !entry.State.Terminal():
		view.State = ToolEntryCompleted
		view.Result = ToolResultView{Content: []ContentPart{{Type: ContentPartText, Text: renderOpenToolText}}}
	default:
		view.State = entry.State
		view.Result.IsError = entry.State != ToolEntryCompleted
		if entry.Tool.Result != nil {
			content, err := r.parts(ctx, entry.Tool.Result.Content)
			if err == nil {
				content = fitToolResultView(content)
			}
			if err != nil {
				return SessionEntryView{}, err
			}
			view.Result.Content = content
		}
	}
	return SessionEntryView{ID: entry.ID, Type: SessionEntryTool, CreatedAt: entry.CreatedAt, Tool: view}, nil
}

// parts copies content parts, resolving image references. Text, json and
// file parts pass through.
func (r *sessionRenderer) parts(ctx context.Context, parts []ContentPart) ([]ContentPart, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	out := make([]ContentPart, 0, len(parts))
	for _, part := range parts {
		if part.Type != ContentPartImage {
			if part.Type == ContentPartJSON {
				part.Value = compactJSONValue(part.Value)
			} else {
				part.Value = cloneRaw(part.Value)
			}
			part.DataBase64 = ""
			out = append(out, part)
			continue
		}
		if !r.opts.ImageInput {
			out = append(out, ContentPart{Type: ContentPartText, Text: renderImageOmittedText(part.MediaType)})
			continue
		}
		resolved, err := r.image(ctx, part)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			out = append(out, ContentPart{Type: ContentPartText, Text: renderImageUnavailableText(part.MediaType)})
			continue
		}
		out = append(out, *resolved)
	}
	return out, nil
}

// image resolves one image reference to a view part with DataBase64, or nil
// when the bytes are missing or do not pass the hosted media rules.
func (r *sessionRenderer) image(ctx context.Context, part ContentPart) (*ContentPart, error) {
	if !renderSupportedImage(part.MediaType) {
		return nil, nil
	}
	switch part.Detail {
	case "", "auto", "low", "high":
	default:
		return nil, nil
	}
	var key string
	switch {
	case part.ActionCallID != "":
		key = "call:" + part.ActionCallID
	case part.ArtifactID != "":
		key = "artifact:" + part.ArtifactID + "@" + part.ArtifactVersion
	default:
		return nil, nil
	}
	cached, seen := r.images[key]
	if !seen {
		var (
			data []byte
			err  error
		)
		if part.ActionCallID != "" {
			data, err = r.callImage(ctx, part.ActionCallID)
		} else {
			data, err = r.artifactImage(ctx, part)
		}
		if err != nil {
			return nil, err
		}
		if data != nil {
			digest := sha256.Sum256(data)
			cached = &ContentPart{DataBase64: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(digest[:])}
		}
		r.images[key] = cached
	}
	if cached == nil || (part.SHA256 != "" && !strings.EqualFold(strings.TrimPrefix(part.SHA256, "sha256:"), cached.SHA256)) {
		return nil, nil
	}
	resolved := part
	resolved.SHA256 = cached.SHA256
	resolved.DataBase64 = cached.DataBase64
	return &resolved, nil
}

func renderSupportedImage(mediaType string) bool {
	return mediaType == "image/png" || mediaType == "image/jpeg" || mediaType == "image/webp"
}

// renderImageBytesValid is the hosted media size rule: 1 byte .. 1 MiB.
func renderImageBytesValid(data []byte) bool {
	return len(data) > 0 && len(data) <= maxModelStepMediaBytes
}

// callImage reads output.modelMedia of a governed call.
func (r *sessionRenderer) callImage(ctx context.Context, callID string) ([]byte, error) {
	if r.opts.Actions == nil {
		return nil, nil
	}
	call, err := r.opts.Actions.GetActionCall(ctx, r.scope, callID)
	if errors.Is(err, ErrActionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("render image of action call %s: %w", callID, err)
	}
	if call == nil {
		return nil, nil
	}
	media, ok := call.Output["modelMedia"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	encoded, _ := media["contentBase64"].(string)
	mediaType, _ := media["mediaType"].(string)
	if !renderSupportedImage(strings.TrimSpace(mediaType)) {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || !renderImageBytesValid(data) {
		return nil, nil
	}
	return data, nil
}

// artifactImage reads an artifact version's bytes and checks size, digest
// and media type against the artifact record.
func (r *sessionRenderer) artifactImage(ctx context.Context, part ContentPart) ([]byte, error) {
	if r.opts.Artifacts == nil || r.opts.Content == nil {
		return nil, nil
	}
	version, err := strconv.ParseInt(strings.TrimSpace(part.ArtifactVersion), 10, 64)
	if err != nil || version <= 0 {
		return nil, nil
	}
	artifact, err := r.opts.Artifacts.GetArtifact(ctx, r.scope, part.ArtifactID, version)
	if errors.Is(err, ErrArtifactNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("render image of artifact %s v%d: %w", part.ArtifactID, version, err)
	}
	if artifact == nil || artifact.SizeBytes <= 0 || artifact.SizeBytes > maxModelStepMediaBytes {
		return nil, nil
	}
	if mediaType, _, err := mime.ParseMediaType(artifact.MediaType); err != nil || mediaType != part.MediaType {
		return nil, nil
	}
	reader, err := r.opts.Content.Open(ctx, artifact.Scope, artifact.ContentRef)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, nil
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxModelStepMediaBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, nil
	}
	if int64(len(data)) != artifact.SizeBytes || !renderImageBytesValid(data) {
		return nil, nil
	}
	if !strings.EqualFold(fmt.Sprintf("sha256:%x", sha256.Sum256(data)), artifact.Digest) {
		return nil, nil
	}
	return data, nil
}
