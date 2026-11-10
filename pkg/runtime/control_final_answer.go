package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// turn_final_answer (section 04 §11, SPEC §3.3 step 1). This file owns the
// tool schema, the strict decode, the plain-text final (an assistant entry with
// no tool calls) and the acceptance pipeline that turns either into the
// FinalAnswer the loop delivers. A failed check is an is_error tool result (or,
// for plain text, a final_rejection note) and the loop continues; the loop owns
// the bound of 3 consecutive rejections (A9) and the delivery.
//
// Pipeline order: decode → silent → content → skills → visual → registered
// checks (completion checks, evidence review, task review: CTL-15/16) →
// FinalOutputPublisher.ValidateFinalAnswer → finalAnswerPark.

func init() {
	registerControlHandler(ControlToolFinalAnswer, finalAnswerHandler{})
}

// Final answer error codes (stable; the model sees them in the tool result).
const (
	finalSilentInvalidCode      = "silent_invalid"
	finalSilentAfterChangeCode  = "silent_after_change"
	finalAnswerEmptyCode        = "answer_empty"
	finalSkillIdentityCode      = "final_skill_identity_invalid"
	finalVisualInvalidCode      = "visual_invalid"
	finalOutputInvalidCode      = "final_output_invalid"
	finalToolRequiredCode       = "final_tool_required"
	finalEmptyReplyCode         = "empty_answer"
	finalPlainTextAttemptsLimit = 3 // the 3rd plain-text attempt where a final tool is required fails the Run
	finalEmptyReplyAttemptLimit = 3 // the 3rd empty plain-text reply fails the Run
)

// Keys of FinalAnswer.Output written by this file. Delivery (CONV-08 /
// runOutputFinalHandler) reads them through the FinalAnswer accessors below.
const (
	finalOutputSummaryKey = "summary"
	finalOutputReportKey  = "report"
	// finalOutputFilesKey lists the ids of the files saved in this Run that
	// the answer hands over; only those go out with it.
	finalOutputFilesKey           = "files"
	finalOutputSilentKey          = "silent"
	finalOutputBroadcastKey       = "broadcastToChannel"
	finalOutputReactedKey         = "reacted"
	finalOutputVisualKey          = "visual"
	finalOutputVisualRejectionKey = "visualRejection"
	finalOutputSkillSelectionsKey = "skillSelections"
)

// finalAnswerArgs are the kernel fields of turn_final_answer. Host extension
// fields (FinalOutputPublisher) are kept apart in finalAnswerCall.Extra.
type finalAnswerArgs struct {
	Answer             string               `json:"answer"`
	AppliedSkillRefs   []string             `json:"appliedSkillRefs,omitempty"`
	Visual             *presentation.Visual `json:"visual,omitempty"`
	BroadcastToChannel bool                 `json:"broadcastToChannel,omitempty"`
	Report             string               `json:"report,omitempty"`
	Silent             bool                 `json:"silent,omitempty"`
	Files              []string             `json:"files,omitempty"`
}

// finalAnswerKernelFields are the property names the kernel owns, whether or
// not a claim offers them. A publisher property with one of these names is
// never offered or routed to the publisher.
var finalAnswerKernelFields = map[string]bool{
	"answer": true, "appliedSkillRefs": true, "visual": true, "broadcastToChannel": true,
	"report": true, "silent": true, "files": true,
}

// finalAnswerCall is the decoded call: the strict kernel arguments, which
// keys were present, and the remaining (host extension) fields.
type finalAnswerCall struct {
	Args    finalAnswerArgs
	Present map[string]bool
	Extra   map[string]json.RawMessage
}

// decodeFinalAnswerCall decodes raw as one JSON object. Kernel fields are
// decoded strictly by type; every other key is kept in Extra and checked
// against the claim's offered fields when the call settles (the dispatcher's
// decode does not see the Run).
func decodeFinalAnswerCall(raw json.RawMessage) (*finalAnswerCall, error) {
	var fields map[string]json.RawMessage
	if err := decodeControlArguments(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("arguments must be one JSON object")
	}
	call := &finalAnswerCall{Present: map[string]bool{}, Extra: map[string]json.RawMessage{}}
	kernel := map[string]json.RawMessage{}
	for key, value := range fields {
		if finalAnswerKernelFields[key] {
			if key == "appliedSkillRefs" {
				value = finalSkillRefsList(value)
			}
			kernel[key] = value
			call.Present[key] = true
			continue
		}
		call.Extra[key] = value
	}
	encoded, err := json.Marshal(kernel)
	if err != nil {
		return nil, err
	}
	if err := decodeControlArguments(encoded, &call.Args); err != nil {
		return nil, err
	}
	return call, nil
}

// finalSkillRefsList takes one Skill ref sent as a bare string as a list of
// one: models sometimes drop the brackets on a single ref, and failing the
// whole final answer for that costs a step. Anything else is left for the
// strict decode.
func finalSkillRefsList(value json.RawMessage) json.RawMessage {
	var single string
	if json.Unmarshal(value, &single) != nil {
		return value
	}
	refs := []string{}
	if strings.TrimSpace(single) != "" {
		refs = append(refs, single)
	}
	encoded, err := json.Marshal(refs)
	if err != nil {
		return value
	}
	return encoded
}

// ---------------------------------------------------------------------------
// Schema (§11.1; constant per claim)

// finalFieldOffered says whether the claim offers a kernel property.
func finalFieldOffered(field string, a ControlAuthority) bool {
	mode := a.finalMode()
	switch field {
	case "answer":
		return true
	case "appliedSkillRefs":
		return len(a.SkillPromptRefs) > 0
	case "visual":
		return a.VisualAllowed
	case "broadcastToChannel":
		return a.BroadcastAllowed
	case "report":
		return mode == FinalModeDefault
	case "silent":
		return mode == FinalModeSpoken || mode == FinalModeDefault && a.RoutineOccurrence
	case "files":
		return mode == FinalModeDefault
	}
	return false
}

// finalRequiredFields lists the required kernel properties in schema order.
func finalRequiredFields(a ControlAuthority) []string {
	required := []string{"answer"}
	if len(a.SkillPromptRefs) > 0 {
		required = append(required, "appliedSkillRefs")
	}
	switch a.finalMode() {
	case FinalModeSpoken:
		required = append(required, "silent")
	}
	return required
}

func finalAnswerSchema(a ControlAuthority) map[string]interface{} {
	answer := map[string]interface{}{"type": "string", "description": "The user-facing answer."}
	if a.finalMode() == FinalModeSpoken {
		answer["description"] = "Only the words to speak; empty when silent is true."
	}
	properties := map[string]interface{}{"answer": answer}
	if finalFieldOffered("appliedSkillRefs", a) {
		properties["appliedSkillRefs"] = map[string]interface{}{
			"type": "array", "uniqueItems": true,
			"description": "The references of the Skill prompts this answer actually used.",
			// No enum: the refs change whenever a Skill is connected, and
			// finalSkillSelections refuses an unknown ref.
			"items": map[string]interface{}{"type": "string"},
		}
	}
	if finalFieldOffered("visual", a) {
		visual := presentation.VisualJSONSchema()
		visual["description"] = presentation.VisualFieldDescription()
		properties["visual"] = visual
	}
	if finalFieldOffered("broadcastToChannel", a) {
		properties["broadcastToChannel"] = map[string]interface{}{
			"type":        "boolean",
			"description": "Your reply goes to the thread. Set true only when it adds channel-wide information that should also appear in the main timeline.",
		}
	}
	if finalFieldOffered("report", a) {
		properties["report"] = map[string]interface{}{
			"type":        "string",
			"description": "The complete human-readable deliverable when it is longer than the answer; shown instead of answer.",
		}
	}
	if finalFieldOffered("files", a) {
		properties["files"] = map[string]interface{}{
			"type": "array", "uniqueItems": true, "items": map[string]interface{}{"type": "string"},
			"description": "Ids of the files this run saved that this answer hands over (the artifactRefs ids a tool returned, never paths): the finished result, such as the PDF or the final image. " +
				"Leave out files made only to build another one (drafts, panels, intermediate images); they stay in the Library and are not shown.",
		}
	}
	if finalFieldOffered("silent", a) {
		description := "Write before answer. True when ambient speech needs no response; answer must then be empty. Do not announce silence."
		if a.finalMode() != FinalModeSpoken {
			description = "Set true, with an empty answer, when nothing needs the person's attention; nothing is posted."
		}
		properties["silent"] = map[string]interface{}{"type": "boolean", "description": description}
	}
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"required": finalRequiredFields(a), "properties": properties,
	}
}

func finalAnswerDescription(a ControlAuthority) string {
	text := "Finish with the user-facing answer"
	if a.VisualAllowed {
		text += ", an optional visual shown under it"
	}
	if len(a.SkillPromptRefs) > 0 {
		text += ", and the prompt Skills used"
	}
	return text + ". Call it alone, after your other tools have returned."
}

// finalOutputFieldNames are the publisher's extension properties offered on
// this Run (kernel names excluded).
func finalOutputFieldNames(run *AgentRun, publisher FinalOutputPublisher) map[string]interface{} {
	if publisher == nil || run == nil {
		return nil
	}
	properties := publisher.FinalAnswerProperties(run)
	offered := map[string]interface{}{}
	for name, schema := range properties {
		if !finalAnswerKernelFields[name] && strings.TrimSpace(name) != "" {
			offered[name] = schema
		}
	}
	if len(offered) == 0 {
		return nil
	}
	return offered
}

// WithFinalOutputProperties adds the FinalOutputPublisher's extension
// properties of run to the turn_final_answer tool in tools (prep-2 calls it on
// the ControlModelStepTools list). They are never required. Other tools and the
// input slice are left unchanged.
func WithFinalOutputProperties(tools []ModelStepTool, run *AgentRun, publisher FinalOutputPublisher) []ModelStepTool {
	extra := finalOutputFieldNames(run, publisher)
	if len(extra) == 0 {
		return tools
	}
	out := make([]ModelStepTool, len(tools))
	copy(out, tools)
	for i, tool := range out {
		if tool.Name != string(ControlToolFinalAnswer) {
			continue
		}
		schema := cloneMap(tool.InputSchema)
		properties := map[string]interface{}{}
		if existing, ok := schema["properties"].(map[string]interface{}); ok {
			for key, value := range existing {
				properties[key] = value
			}
		}
		for key, value := range extra {
			properties[key] = value
		}
		schema["properties"] = properties
		out[i].InputSchema = schema
	}
	return out
}

type finalAnswerHandler struct{}

func (finalAnswerHandler) spec(a ControlAuthority) ControlToolSpec {
	return ControlToolSpec{Description: finalAnswerDescription(a), InputSchema: finalAnswerSchema(a)}
}

func (finalAnswerHandler) decode(raw json.RawMessage) (interface{}, error) {
	return decodeFinalAnswerCall(raw)
}

func (finalAnswerHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	call, ok := cc.Arguments.(*finalAnswerCall)
	if !ok || call == nil {
		return ControlResult{}, errors.New("turn_final_answer arguments were not decoded")
	}
	if message := checkFinalAnswerFields(call, cc); message != "" {
		return x.refuse(ctx, cc, controlArgumentsInvalid(message))
	}
	draft := &finalAnswerDraft{Args: call.Args, Extra: call.Extra}
	return x.acceptFinalAnswer(ctx, cc, draft)
}

// checkFinalAnswerFields: every present field must be offered on this claim
// and every required field present. It returns the decoder-style message, or "".
func checkFinalAnswerFields(call *finalAnswerCall, cc ControlContext) string {
	var unknown []string
	for key := range call.Present {
		if !finalFieldOffered(key, cc.Authority) {
			unknown = append(unknown, key)
		}
	}
	var publisher FinalOutputPublisher
	if cc.Binding != nil {
		publisher = cc.Binding.FinalOutput
	}
	extensions := finalOutputFieldNames(cc.Run, publisher)
	for key := range call.Extra {
		if _, ok := extensions[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Sprintf("json: unknown field %q", unknown[0])
	}
	for _, key := range finalRequiredFields(cc.Authority) {
		if !call.Present[key] {
			return fmt.Sprintf("missing required field %q", key)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Acceptance pipeline (§11.3)

// finalAnswerDraft is a final answer under validation. Checks may change
// Output (evidence grounding adds its review) and Visual.
type finalAnswerDraft struct {
	Args            finalAnswerArgs
	Extra           map[string]json.RawMessage
	PlainText       bool
	ReactionsOnly   bool
	Visual          *presentation.Visual
	VisualRejection string
	SkillSelections []HostedSkillSelection
	Output          map[string]interface{}
}

// content is the user-visible text: the trimmed report when present, else the
// trimmed answer (the precedence chat replies have always used).
func (d *finalAnswerDraft) content() string {
	if report := strings.TrimSpace(d.Args.Report); report != "" {
		return report
	}
	return strings.TrimSpace(d.Args.Answer)
}

// finalAnswerRejection is a check's verdict: an is_error result the model
// can correct (Failure nil), or a failure of the Run (Failure set; the
// rejection is still recorded on the entry or as the note).
type finalAnswerRejection struct {
	Code    string
	Message string
	Extra   map[string]interface{}
	Failure *LoopFailure
}

// finalAnswerCheck is one registered completion check. CTL-15 (completion
// checks, evidence grounding) and CTL-16 (task completion review) register
// theirs from their own files' init(), with these orders.
type finalAnswerCheck struct {
	Order int
	Name  string
	Check func(ctx context.Context, x *ControlExecutor, cc ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error)
}

const (
	finalCheckOrderCompletion = 100 // §11.4 1-3: URLs, external claims, approval claims
	finalCheckOrderTaskReview = 300 // §11.6: task completion review
)

var finalAnswerChecks []finalAnswerCheck

func registerFinalAnswerCheck(check finalAnswerCheck) {
	if check.Check == nil || strings.TrimSpace(check.Name) == "" {
		panic("register final answer check without a name or function")
	}
	for _, existing := range finalAnswerChecks {
		if existing.Name == check.Name {
			panic(fmt.Sprintf("final answer check %q registered twice", check.Name))
		}
	}
	finalAnswerChecks = append(finalAnswerChecks, check)
	sort.SliceStable(finalAnswerChecks, func(i, j int) bool { return finalAnswerChecks[i].Order < finalAnswerChecks[j].Order })
}

// acceptFinalAnswer runs the pipeline on a decoded final. It returns
// ControlFinal with the FinalAnswer (the tool entry stays open; the loop marks
// it delivered), ControlContinue after a rejection, or ControlFailed.
func (x *ControlExecutor) acceptFinalAnswer(ctx context.Context, cc ControlContext, d *finalAnswerDraft) (ControlResult, error) {
	mode := cc.Authority.finalMode()
	answer := strings.TrimSpace(d.Args.Answer)
	// 2. Silent: a spoken reply or a routine occurrence, with an empty
	// answer and no visual, report, files or extension fields. A routine occurrence that changed
	// something always reports it.
	if d.Args.Silent {
		routine := mode == FinalModeDefault && cc.Authority.RoutineOccurrence
		if (mode != FinalModeSpoken && !routine) || answer != "" || strings.TrimSpace(d.Args.Report) != "" || d.Args.Visual != nil ||
			len(d.Args.Files) > 0 || len(d.Extra) > 0 {
			message := "silent is only for a spoken reply that needs no response: leave answer empty and send no visual or files."
			if routine {
				message = "silent means nothing is posted: leave answer and report empty and send no visual or files."
			}
			return x.rejectFinal(ctx, cc, d, &finalAnswerRejection{Code: finalSilentInvalidCode, Message: message})
		}
		if routine {
			changed, err := x.runChangedSomething(ctx, cc)
			if err != nil {
				return ControlResult{}, err
			}
			if changed {
				return x.rejectFinal(ctx, cc, d, &finalAnswerRejection{Code: finalSilentAfterChangeCode,
					Message: "This run changed something, so report what it did."})
			}
		}
	}
	// 3. Content, unless silent or reactions only.
	if !d.Args.Silent && d.content() == "" {
		reacted, err := x.runReacted(ctx, cc)
		if err != nil {
			return ControlResult{}, err
		}
		if !reacted || d.Args.Visual != nil {
			return x.rejectFinal(ctx, cc, d, &finalAnswerRejection{Code: finalAnswerEmptyCode,
				Message: "The answer is empty; write the reply in answer."})
		}
		d.ReactionsOnly = true
	}
	// 4. Skills.
	selections, rejection := finalSkillSelections(d.Args.AppliedSkillRefs, cc.Authority.SkillPromptRefs)
	if rejection != nil {
		return x.rejectFinal(ctx, cc, d, rejection)
	}
	d.SkillSelections = selections
	// 5. Visual: structure here; the host's binding/link/image/place checks
	// and place stamping run where the visual is stored (delivery), which
	// answers retry_final when they fail.
	if d.Args.Visual != nil {
		visual := cloneVisual(d.Args.Visual)
		if _, err := presentation.Validate(ctx, visual.Surface(presentation.StateReady), presentation.ValidateOptions{SkipHostChecks: true}); err != nil {
			prior, countErr := x.priorFinalErrorCodes(ctx, cc)
			if countErr != nil {
				return ControlResult{}, countErr
			}
			if prior[finalVisualInvalidCode] == 0 {
				return x.rejectFinal(ctx, cc, d, &finalAnswerRejection{Code: finalVisualInvalidCode,
					Message: fmt.Sprintf("visual: %v. Fix the visual or leave it out.", err)})
			}
			// The second invalid visual in the Run never costs the answer.
			d.VisualRejection = "visual: " + err.Error()
		} else {
			d.Visual = visual
		}
	}
	d.Output = d.output(mode)
	// 6-8. Completion checks, evidence review, task review.
	for _, check := range finalAnswerChecks {
		rejection, err := check.Check(ctx, x, cc, d)
		if err != nil {
			return ControlResult{}, fmt.Errorf("final answer check %s: %w", check.Name, err)
		}
		if rejection != nil {
			return x.rejectFinal(ctx, cc, d, rejection)
		}
	}
	// 9. Host extension fields.
	if len(d.Extra) > 0 {
		var publisher FinalOutputPublisher
		if cc.Binding != nil {
			publisher = cc.Binding.FinalOutput
		}
		if publisher == nil {
			return ControlResult{}, errors.New("turn_final_answer has extension fields but the binding has no FinalOutputPublisher")
		}
		if err := publisher.ValidateFinalAnswer(cc.Run, cloneRawFields(d.Extra)); err != nil {
			return x.rejectFinal(ctx, cc, d, &finalAnswerRejection{Code: finalOutputInvalidCode, Message: err.Error()})
		}
	}
	final := &FinalAnswer{PlainText: d.PlainText, Answer: d.content(), Output: d.Output}
	if d.Args.Silent || d.ReactionsOnly {
		final.Answer = ""
	}
	if cc.Step != nil {
		final.StepID = cc.Step.ID
	}
	if !d.PlainText && cc.Entry != nil {
		final.ToolEntryID, final.ToolCallID, final.StepID = cc.Entry.ID, cc.Entry.ToolCallID, cc.Entry.StepID
	}
	// 10. OTP wait or pending setup (section 07 §8).
	park, err := x.finalPark(ctx, cc, final.Answer)
	if err != nil {
		return ControlResult{}, err
	}
	final.Park = park
	return ControlResult{Kind: ControlFinal, Run: cc.Run, Entry: cc.Entry, Final: final}, nil
}

// output is FinalAnswer.Output (§11.3 RunOutput).
func (d *finalAnswerDraft) output(mode FinalAnswerMode) map[string]interface{} {
	if d.Args.Silent {
		return map[string]interface{}{finalOutputSilentKey: true}
	}
	if d.ReactionsOnly {
		return map[string]interface{}{finalOutputReactedKey: true}
	}
	output := map[string]interface{}{finalOutputSummaryKey: strings.TrimSpace(d.Args.Answer)}
	if report := strings.TrimSpace(d.Args.Report); report != "" {
		output[finalOutputReportKey] = report
	}
	if d.Args.BroadcastToChannel {
		output[finalOutputBroadcastKey] = true
	}
	if files := handedOverFileIDs(d.Args.Files); len(files) > 0 {
		output[finalOutputFilesKey] = files
	}
	if d.Visual != nil {
		output[finalOutputVisualKey] = d.Visual
	}
	if d.VisualRejection != "" {
		output[finalOutputVisualRejectionKey] = d.VisualRejection
	}
	if len(d.SkillSelections) > 0 {
		output[finalOutputSkillSelectionsKey] = d.SkillSelections
	}
	return output
}

// finalSkillSelections builds the dispositions of the offered prompt Skills
// (cortex hostedNativeFinalOutcome rule): each offered ref is applied when
// cited, otherwise not applied; duplicates and unknown refs are refused.
func finalSkillSelections(applied, offered []string) ([]HostedSkillSelection, *finalAnswerRejection) {
	cited := make(map[string]bool, len(applied))
	for _, ref := range applied {
		if cited[ref] {
			return nil, &finalAnswerRejection{Code: finalSkillIdentityCode, Message: fmt.Sprintf("appliedSkillRefs lists %q twice.", ref)}
		}
		cited[ref] = true
	}
	selections := make([]HostedSkillSelection, 0, len(offered))
	for _, ref := range offered {
		selection := HostedSkillSelection{SkillRef: ref, Disposition: HostedSkillNotApplied, Summary: "Not used in this answer."}
		if cited[ref] {
			selection.Disposition, selection.Summary = HostedSkillApplied, "Used in this answer."
			delete(cited, ref)
		}
		selections = append(selections, selection)
	}
	if len(cited) > 0 {
		unknown := make([]string, 0, len(cited))
		for ref := range cited {
			unknown = append(unknown, ref)
		}
		sort.Strings(unknown)
		return nil, &finalAnswerRejection{Code: finalSkillIdentityCode,
			Message: fmt.Sprintf("appliedSkillRefs cites %q, which is not a Skill prompt loaded in this run.", unknown[0])}
	}
	return selections, nil
}

// rejectFinal records a rejection: the tool entry → error with the code (and
// ControlEntryMeta.ErrorCode), or for plain text a final_rejection note. A
// rejection with a Failure then fails the Run.
func (x *ControlExecutor) rejectFinal(ctx context.Context, cc ControlContext, d *finalAnswerDraft, r *finalAnswerRejection) (ControlResult, error) {
	var recorded *SessionEntry
	if d.PlainText {
		note, err := x.appendFinalRejectionNote(ctx, cc, r.Code, r.Message)
		if err != nil {
			return ControlResult{}, err
		}
		recorded = note
	} else {
		entry, err := x.transitionControlEntry(ctx, cc, ToolEntryError, nil,
			controlErrorResult(ToolResultFailed, r.Code, r.Message, r.Extra), &ControlEntryMeta{ErrorCode: r.Code})
		if err != nil {
			return ControlResult{}, err
		}
		recorded = entry
	}
	if r.Failure != nil {
		return ControlResult{Kind: ControlFailed, Run: cc.Run, Entry: recorded, Failure: r.Failure}, nil
	}
	return ControlResult{Kind: ControlContinue, Run: cc.Run, Entry: recorded}, nil
}

// finalRejectionNoteData is the kernel data of a final_rejection note.
type finalRejectionNoteData struct {
	ErrorCode string `json:"errorCode"`
}

// appendFinalRejectionNote appends the step's final_rejection note (one per
// step: id and SourceKey final-rejection:<stepId>).
func (x *ControlExecutor) appendFinalRejectionNote(ctx context.Context, cc ControlContext, code, message string) (*SessionEntry, error) {
	sessionID := cc.Run.SessionID
	if cc.Session != nil {
		sessionID = cc.Session.ID
	}
	if sessionID == "" || cc.Step == nil || cc.Step.ID == "" {
		return nil, errors.New("plain-text final needs the Run's session and step")
	}
	data, err := json.Marshal(finalRejectionNoteData{ErrorCode: code})
	if err != nil {
		return nil, err
	}
	key := FinalRejectionEntryKey(cc.Step.ID)
	note := &SessionEntry{
		ID: SessionEntryID(sessionID, key), Scope: cc.Run.Scope, SessionID: sessionID, RunID: cc.Run.ID,
		StepID: cc.Step.ID, Step: cc.Step.Sequence, Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteFinalRejection, Text: message, Data: data},
	}
	now := x.now()
	lease := cc.lease(now)
	appended, err := x.Sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: cc.Run.Scope, SessionID: sessionID, RunID: cc.Run.ID, Lease: &lease,
		Entries: []*SessionEntry{note}, Now: now,
	})
	if err != nil {
		return nil, err
	}
	if len(appended.Entries) == 1 {
		return appended.Entries[0], nil
	}
	// Replayed or skipped by SourceKey: the note of this step already exists.
	return x.Sessions.GetSessionEntry(ctx, cc.Run.Scope, note.ID)
}

// priorFinalErrorCodes counts the rejection codes of this Run's earlier final
// attempts: final tool entries (ControlEntryMeta.ErrorCode) and final_rejection
// notes. The current entry and the current step's note are not counted. CTL-15/16 use it for their
// "second time" rules.
func (x *ControlExecutor) priorFinalErrorCodes(ctx context.Context, cc ControlContext) (map[string]int, error) {
	counts := map[string]int{}
	tools, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, ToolNames: []string{string(ControlToolFinalAnswer)}, Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, entry := range tools {
		if cc.Entry != nil && entry.ID == cc.Entry.ID {
			continue
		}
		meta, err := DecodeControlEntryMeta(entry)
		if err != nil {
			return nil, err
		}
		if meta != nil && meta.ErrorCode != "" {
			counts[meta.ErrorCode]++
		}
	}
	notes, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, NoteKinds: []NoteKind{NoteFinalRejection}, Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, entry := range notes {
		// The current step's own note (a replay after a crash) is not prior.
		if entry.Note == nil || len(entry.Note.Data) == 0 || cc.Step != nil && entry.StepID == cc.Step.ID {
			continue
		}
		var data finalRejectionNoteData
		if err := json.Unmarshal(entry.Note.Data, &data); err != nil {
			return nil, fmt.Errorf("decode final rejection note %s: %w", entry.ID, err)
		}
		if data.ErrorCode != "" {
			counts[data.ErrorCode]++
		}
	}
	return counts, nil
}

// runReacted: a react call of this Run completed (reactions-only reply).
func (x *ControlExecutor) runReacted(ctx context.Context, cc ControlContext) (bool, error) {
	entries, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, ToolNames: []string{string(ControlToolReact)},
		States: []ToolEntryState{ToolEntryCompleted}, Limit: 1,
	})
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

// runChangedSomethingStatuses are the action call states in which the call
// ran: compensation undoes a change that still happened.
var runChangedSomethingStatuses = []ActionCallStatus{
	ActionCallStatusSucceeded, ActionCallStatusCompensating, ActionCallStatusCompensated,
}

// runChangedSomething reports whether the Run, or any Run under the same
// root (a delegated or forked child), completed an action whose side effect
// is not none or read, granted or approved. It runs only for a silent routine
// final, never on a step.
func (x *ControlExecutor) runChangedSomething(ctx context.Context, cc ControlContext) (bool, error) {
	actions, ok := x.Portfolio.(ActionStore)
	if !ok {
		return false, errors.New("control executor portfolio is not an action store; a silent routine final needs the Run's calls")
	}
	const pageSize = 200
	rootID := strings.TrimSpace(cc.Run.RootRunID)
	if rootID == "" {
		rootID = cc.Run.ID
	}
	runIDs := []string{cc.Run.ID}
	for offset := 0; ; offset += pageSize {
		runs, err := x.Portfolio.ListAgentRuns(ctx, AgentRunFilter{Scope: cc.Run.Scope, RootRunID: rootID, Limit: pageSize, Offset: offset})
		if err != nil {
			return false, err
		}
		for _, run := range runs {
			if run != nil && run.ID != cc.Run.ID {
				runIDs = append(runIDs, run.ID)
			}
		}
		if len(runs) < pageSize {
			break
		}
	}
	for _, runID := range runIDs {
		for offset := 0; ; offset += pageSize {
			calls, err := actions.ListActionCalls(ctx, ActionFilter{
				Scope: cc.Run.Scope, RunID: runID, Status: runChangedSomethingStatuses, Limit: pageSize, Offset: offset,
			})
			if err != nil {
				return false, err
			}
			for _, call := range calls {
				if call != nil && call.SideEffect != skill.SideEffectNone && call.SideEffect != skill.SideEffectRead {
					return true, nil
				}
			}
			if len(calls) < pageSize {
				break
			}
		}
	}
	return false, nil
}

// finalPark applies finalAnswerPark to the Run's governed calls on its path.
func (x *ControlExecutor) finalPark(ctx context.Context, cc ControlContext, answer string) (*FinalPark, error) {
	path := cc.Path
	if path == nil {
		sessionID := cc.Run.SessionID
		if cc.Session != nil {
			sessionID = cc.Session.ID
		}
		if sessionID == "" {
			return nil, nil
		}
		loaded, err := x.Sessions.LoadAgentSessionPath(ctx, cc.Run.Scope, sessionID)
		if err != nil {
			return nil, err
		}
		path = loaded
	}
	governed := path.Compaction != nil && path.Compaction.Compaction != nil && len(path.Compaction.Compaction.ActionCallIDs) > 0
	for _, entry := range path.Entries {
		if entry != nil && entry.RunID == cc.Run.ID && entry.ActionCallID != "" {
			governed = true
			break
		}
	}
	if !governed {
		return nil, nil
	}
	actions, ok := x.Portfolio.(ActionStore)
	if !ok {
		return nil, errors.New("control executor portfolio is not an action store; the final answer park needs the Run's calls")
	}
	views, err := loadGovernedToolViews(ctx, actions, cc.Run.Scope, path, cc.Run.ID)
	if err != nil {
		return nil, err
	}
	plan := finalAnswerPark(cc.Run, views, cc.Authority.CanAskUser, answer, cc.Run.PendingInterventions)
	if plan == nil {
		return nil, nil
	}
	return &FinalPark{Deliver: plan.Deliver, Park: LoopPark{Status: plan.Status, Wake: plan.Wake, Summary: plan.Summary}}, nil
}

func cloneRawFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Plain-text final (§11.2)

// PlainTextFinal handles an assistant entry with no tool calls (SPEC §3.1
// phase 8). cc.Entry is nil; cc.Assistant is that entry and cc.Step its step
// (still running). Results:
//   - ControlFinal: an accepted final (FinalAnswer.PlainText unless the text
//     was a turn_final_answer JSON object, which is accepted as the tool's
//     arguments);
//   - ControlContinue: a final_rejection note was appended (Entry is the
//     note, not a tool entry); the loop counts it toward the rejection bound;
//   - ControlFailed: the 3rd plain reply where a final tool is required, or
//     the 3rd empty reply.
func (x *ControlExecutor) PlainTextFinal(ctx context.Context, cc ControlContext, text string) (ControlResult, error) {
	if x == nil || x.Sessions == nil {
		return ControlResult{}, errors.New("control executor has no session store")
	}
	if cc.Run == nil || cc.Step == nil {
		return ControlResult{}, errors.New("plain-text final needs its Run and step")
	}
	if cc.Run.Status != AgentRunStatusRunning || cc.WorkerID == "" || cc.Run.LeaseOwner != cc.WorkerID {
		return ControlResult{}, ErrLeaseLost
	}
	if cc.Assistant != nil && cc.Assistant.Assistant != nil && len(cc.Assistant.Assistant.ToolCalls) > 0 {
		return ControlResult{}, errors.New("plain-text final on an assistant entry with tool calls")
	}
	cc.Entry = nil
	mode := cc.Authority.finalMode()
	finalOffered := controlToolOffered(ControlToolFinalAnswer, cc.Authority)
	trimmed := strings.TrimSpace(text)
	// 1. Repair: the text is the tool's arguments as one JSON object (kernel
	// fields only: extension fields need a tool entry to be published from).
	if finalOffered && strings.HasPrefix(trimmed, "{") {
		if call, err := decodeFinalAnswerCall(json.RawMessage(trimmed)); err == nil && call.Present["answer"] &&
			len(call.Extra) == 0 && checkFinalAnswerFields(call, cc) == "" {
			return x.acceptFinalAnswer(ctx, cc, &finalAnswerDraft{Args: call.Args, Extra: call.Extra, PlainText: true})
		}
	}
	// 2. Modes that need a structured final tool.
	if tool := finalToolRequired(mode); tool != "" {
		prior, err := x.priorFinalErrorCodes(ctx, cc)
		if err != nil {
			return ControlResult{}, err
		}
		message := fmt.Sprintf("Finish with %s (%s); a plain reply is not accepted in this run.", tool, finalToolRequiredFields(tool, cc.Authority))
		rejection := &finalAnswerRejection{Code: finalToolRequiredCode, Message: message}
		if prior[finalToolRequiredCode]+1 >= finalPlainTextAttemptsLimit {
			rejection.Failure = &LoopFailure{Kind: RunFailureKindExecution, Code: finalToolRequiredCode,
				Message: fmt.Sprintf("The agent replied without %s %d times, so the run stopped.", tool, finalPlainTextAttemptsLimit)}
		}
		return x.rejectFinal(ctx, cc, &finalAnswerDraft{PlainText: true}, rejection)
	}
	draft := &finalAnswerDraft{Args: finalAnswerArgs{Answer: trimmed}, PlainText: true}
	// 4. Empty text.
	if trimmed == "" {
		if mode == FinalModeSpoken {
			draft.Args.Silent = true
			return x.acceptFinalAnswer(ctx, cc, draft)
		}
		reacted, err := x.runReacted(ctx, cc)
		if err != nil {
			return ControlResult{}, err
		}
		if reacted {
			return x.acceptFinalAnswer(ctx, cc, draft)
		}
		prior, err := x.priorFinalErrorCodes(ctx, cc)
		if err != nil {
			return ControlResult{}, err
		}
		rejection := &finalAnswerRejection{Code: finalEmptyReplyCode,
			Message: "An empty reply is not an answer. Write the reply, or call turn_final_answer."}
		if prior[finalEmptyReplyCode]+1 >= finalEmptyReplyAttemptLimit {
			rejection.Failure = &LoopFailure{Kind: RunFailureKindExecution, Code: finalEmptyReplyCode,
				Message: "The agent sent an empty reply three times, so the run stopped."}
		}
		return x.rejectFinal(ctx, cc, draft, rejection)
	}
	// 3. The text is the answer.
	return x.acceptFinalAnswer(ctx, cc, draft)
}

// finalToolRequired names the final tool a plain reply cannot replace in
// mode ("" when plain text is a final answer).
func finalToolRequired(mode FinalAnswerMode) ControlToolName {
	switch mode {
	case FinalModeDecision:
		return ControlToolRequestDecision
	case FinalModeReview:
		return ControlToolCompletionReview
	}
	return ""
}

func finalToolRequiredFields(tool ControlToolName, a ControlAuthority) string {
	if tool == ControlToolFinalAnswer {
		return strings.Join(finalRequiredFields(a), ", ")
	}
	h, ok := controlHandlers[tool]
	if !ok {
		return "its required fields"
	}
	required, _ := h.spec(a).InputSchema["required"].([]string)
	if len(required) == 0 {
		return "its required fields"
	}
	return strings.Join(required, ", ")
}

// ---------------------------------------------------------------------------
// Accessors for delivery (CONV-08, runOutputFinalHandler)

// Silent: a spoken reply that needs no response; nothing is posted.
func (a FinalAnswer) Silent() bool {
	silent, _ := a.Output[finalOutputSilentKey].(bool)
	return silent
}

// ReactionsOnly: the Run's reactions are the reply; no message is posted.
func (a FinalAnswer) ReactionsOnly() bool {
	reacted, _ := a.Output[finalOutputReactedKey].(bool)
	return reacted
}

// BroadcastToChannel: a thread reply that also goes to the main timeline.
func (a FinalAnswer) BroadcastToChannel() bool {
	broadcast, _ := a.Output[finalOutputBroadcastKey].(bool)
	return broadcast
}

// Visual is the structurally valid visual of the answer, or nil. Delivery
// runs the host checks and place stamping, stores it, and removes it from the
// Run output.
func (a FinalAnswer) Visual() *presentation.Visual {
	visual, _ := a.Output[finalOutputVisualKey].(*presentation.Visual)
	return visual
}

// FinalOutputPublicationFor returns the publish request for the host
// extension fields of an accepted turn_final_answer (SPEC §3.3 step 4), or nil
// when the call carried none. entry is the final tool entry.
func FinalOutputPublicationFor(run *AgentRun, answer FinalAnswer, entry *SessionEntry, publisher FinalOutputPublisher) (*FinalOutputPublication, error) {
	if publisher == nil || answer.ToolEntryID == "" {
		return nil, nil
	}
	if entry == nil || entry.Tool == nil || entry.ID != answer.ToolEntryID {
		return nil, errors.New("final output publication needs the final tool entry")
	}
	call, err := decodeFinalAnswerCall(entry.Tool.Arguments)
	if err != nil {
		return nil, err
	}
	extensions := finalOutputFieldNames(run, publisher)
	fields := map[string]json.RawMessage{}
	for key, value := range call.Extra {
		if _, ok := extensions[key]; ok {
			fields[key] = value
		}
	}
	if len(fields) == 0 {
		return nil, nil
	}
	return &FinalOutputPublication{Run: run, StepID: answer.StepID, ToolCallID: answer.ToolCallID, Fields: fields}, nil
}

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
// (its "files"), in the Run's order. A Run's other files stay in the Library.
// unknown lists the entries that name no saved file, such as a path given
// instead of an id.
func handedOverArtifacts(saved []ConversationReference, output map[string]interface{}) (out []ConversationReference, unknown []string) {
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
	wanted = handedOverFileIDs(wanted)
	known := map[string]bool{}
	for _, ref := range saved {
		known[ref.ID] = true
	}
	want := map[string]bool{}
	for _, id := range wanted {
		want[id] = true
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	for _, ref := range saved {
		if want[ref.ID] {
			out = append(out, ref)
		}
	}
	return out, unknown
}

// unknownHandedOverFilesText sends a final whose files name no saved file
// back to the model, which otherwise attaches nothing without a word.
func unknownHandedOverFilesText(unknown []string, saved []ConversationReference) string {
	ids := make([]string, 0, len(saved))
	for _, ref := range saved {
		ids = append(ids, ref.ID)
	}
	if len(ids) == 0 {
		return fmt.Sprintf("files names %s, but this run saved no files. Save the file first (present on the secure VM), or leave files out.", strings.Join(unknown, ", "))
	}
	return fmt.Sprintf("files takes the ids of files this run saved (the artifactRefs ids a tool returned), not paths or names: %s is not one. This run's files: %s.",
		strings.Join(unknown, ", "), strings.Join(ids, ", "))
}
