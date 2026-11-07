package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// LoopCompactor is the kernel StepCompactor (section 08 §2-§3). It replaces
// the older part of a session path with a model-written summary: one
// compaction entry, appended to the Run's own session under the Run lease.
// Compaction is not a step: it writes no agent_turns row and charges no Turns;
// its usage is returned so the loop adds it to the step that triggered it.
type LoopCompactor struct {
	Host     StepHost          // only ExecuteModelStep is used
	Sessions AgentSessionStore // appends the compaction entry and its event
	// Requests builds the step's own model request (prep-2), so the summary
	// call's System and Tools are byte-identical to the step's and it reads
	// the step's prompt cache.
	Requests ModelStepPreparer
	// Budget reserves and settles a bounded Run's compaction usage. nil is
	// valid only while no Run has a budget.
	Budget compactionBudget
	Now    func() time.Time
	NewID  func() string
}

var _ StepCompactor = (*LoopCompactor)(nil)

// LoopFailureError carries a LoopFailure through an error return. The loop
// maps it with errors.As to failStep(failure) instead of producer_failed.
type LoopFailureError struct{ Failure LoopFailure }

func (e *LoopFailureError) Error() string {
	if e == nil {
		return "run loop failure"
	}
	if message := strings.TrimSpace(e.Failure.Message); message != "" {
		return message
	}
	return "run loop failure: " + e.Failure.Code
}

// modelStepSummaryFailed is the host code for a summary call that returned
// tool calls twice or no text (section 06 §10).
const modelStepSummaryFailed = "summary_failed"

// compactionEventType is the activity event appended with every compaction.
const compactionEventType = "session.compacted"

// compactionBudgetReservationID is the Run budget reservation of the
// compaction run before step stepID.
func compactionBudgetReservationID(stepID string) string { return "compaction:" + stepID }

// compactionBudget reserves and settles the usage of one compaction on a
// bounded Run, fenced by the Run lease. run names the Run (Scope, ID); its
// Revision is not trusted, because heartbeats and steering bump it while the
// summary calls run. Both return the updated Run.
type compactionBudget interface {
	ReserveCompaction(ctx context.Context, run *AgentRun, workerID, reservationID string, usage BudgetUsage) (*AgentRun, error)
	SettleCompaction(ctx context.Context, run *AgentRun, workerID, reservationID string, usage BudgetUsage) (*AgentRun, error)
}

// runActivityCompactionBudget is the store-backed compactionBudget: a
// running→running TransitionRun under the lease, like the step reservation.
type runActivityCompactionBudget struct {
	activity *RunActivityService
	now      func() time.Time
}

func newRunActivityCompactionBudget(activity *RunActivityService, now func() time.Time) compactionBudget {
	if now == nil {
		now = time.Now
	}
	return &runActivityCompactionBudget{activity: activity, now: now}
}

// compactionBudgetWriteAttempts bounds the re-read/retry of a budget write
// that loses a revision race against a heartbeat or a steering write.
const compactionBudgetWriteAttempts = 4

func (b *runActivityCompactionBudget) ReserveCompaction(ctx context.Context, run *AgentRun, workerID, reservationID string, usage BudgetUsage) (*AgentRun, error) {
	return b.transition(ctx, run, func(revision int64) RunTransitionRequest {
		return RunTransitionRequest{
			ExpectedRevision: revision, Status: AgentRunStatusRunning, LeaseOwner: workerID,
			Summary: "Reserved capacity to compact the session", EventType: "budget.reserved",
			Actor:             ActivityActor{Type: "worker", ID: workerID},
			BudgetReservation: &BudgetReservation{ID: reservationID, Usage: usage, CreatedAt: b.now()},
		}
	})
}

// SettleCompaction charges the budget only. The compaction's usage reaches
// the step's activity usage through the step that triggered it (section 08
// §2), so no ActivityUsageDelta is written here.
func (b *runActivityCompactionBudget) SettleCompaction(ctx context.Context, run *AgentRun, workerID, reservationID string, usage BudgetUsage) (*AgentRun, error) {
	return b.transition(ctx, run, func(revision int64) RunTransitionRequest {
		delta := usage
		return RunTransitionRequest{
			ExpectedRevision: revision, Status: AgentRunStatusRunning, LeaseOwner: workerID,
			Summary: "Recorded the usage of compacting the session", EventType: "budget.settled",
			Actor:                     ActivityActor{Type: "worker", ID: workerID},
			SettleBudgetReservationID: reservationID,
			BudgetUsageDelta:          &delta,
		}
	})
}

// transition re-reads the Run before each attempt and writes at its current
// revision; the lease owner check in TransitionRun is the real fence.
func (b *runActivityCompactionBudget) transition(ctx context.Context, run *AgentRun, request func(revision int64) RunTransitionRequest) (*AgentRun, error) {
	if b == nil || b.activity == nil || b.activity.portfolio == nil {
		return nil, fmt.Errorf("%w: the compaction budget has no run store", ErrTurnHostConfiguration)
	}
	var err error
	for attempt := 0; attempt < compactionBudgetWriteAttempts; attempt++ {
		var current *AgentRun
		current, err = b.activity.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, ErrRunNotFound
		}
		var updated *AgentRun
		updated, _, err = b.activity.TransitionRun(ctx, run.Scope, run.ID, request(current.Revision))
		if !errors.Is(err, ErrRevisionConflict) {
			return updated, err
		}
	}
	return nil, err
}

// compactionBudgetUsage is the budget charge of a compaction's model calls.
// Turns stay 0 (D12) and duration is the step's, not charged twice.
func compactionBudgetUsage(usage ModelStepUsage) BudgetUsage {
	costMicros := int64(0)
	if usage.Cost > 0 {
		costMicros = int64(math.Round(usage.Cost * 1_000_000))
	}
	return BudgetUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostMicros: costMicros}
}

func addModelStepUsage(total, delta ModelStepUsage) ModelStepUsage {
	total.InputTokens += delta.InputTokens
	total.OutputTokens += delta.OutputTokens
	total.ReasoningTokens += delta.ReasoningTokens
	total.CacheReadTokens += delta.CacheReadTokens
	total.CacheWriteTokens += delta.CacheWriteTokens
	total.Cost += delta.Cost
	total.DurationMS += delta.DurationMS
	total.ProviderDurationMS += delta.ProviderDurationMS
	if total.FirstOutputMS == 0 {
		total.FirstOutputMS = delta.FirstOutputMS
	}
	total.Attempts += delta.Attempts
	return total
}

// compactionPressureKey carries the HostedContextPressure of the length
// rejection that forced an overflow compaction.
type compactionPressureKey struct{}

// withCompactionPressure attaches the context failure's pressure for Compact
// (force). The loop passes TurnHostFailure.ContextPressure: when the path has
// no known window, the overflow compaction sizes its calls from it.
func withCompactionPressure(ctx context.Context, pressure *HostedContextPressure) context.Context {
	if pressure == nil {
		return ctx
	}
	copied := *pressure
	return context.WithValue(ctx, compactionPressureKey{}, &copied)
}

func compactionPressureFrom(ctx context.Context) *HostedContextPressure {
	pressure, _ := ctx.Value(compactionPressureKey{}).(*HostedContextPressure)
	return pressure
}

// compactionFailure is the LoopFailureError of a guard failure code
// (TurnHostContextExceeded or RunFailureCodeCompactionFailed).
func compactionFailure(code string) error {
	failure, _ := guardRunFailureFor(code)
	return &LoopFailureError{Failure: LoopFailure{
		Kind: failure.Kind, Code: failure.Code, Message: failure.Message, Explainable: failure.Explainable,
	}}
}

func compactionBudgetFailure() error {
	return &LoopFailureError{Failure: LoopFailure{
		Kind: "budget", Code: "budget_exhausted",
		Message: "The run's budget has no room left to compact the conversation.",
	}}
}

// compactionModel is the model whose window and anchor the path is measured
// against: the step's model when already known, else the binding's model
// when the path holds a step of it, else the newest step's model when no
// model change followed it (the host resolved the binding's model to it).
func compactionModel(in StepInput) string {
	if in.Step != nil && strings.TrimSpace(in.Step.Model) != "" {
		return in.Step.Model
	}
	requested := ""
	if in.Binding != nil {
		requested = strings.TrimSpace(in.Binding.Model)
	}
	if in.Path == nil {
		return requested
	}
	newest := ""
	for i := len(in.Path.Entries) - 1; i >= 0; i-- {
		entry := in.Path.Entries[i]
		if entry == nil {
			continue
		}
		if entry.Type == SessionEntryModelChange && newest == "" {
			newest = "-"
		}
		if entry.Type != SessionEntryAssistant || entry.Assistant == nil {
			continue
		}
		if requested != "" && entry.Assistant.Model == requested {
			return requested
		}
		if newest == "" {
			newest = entry.Assistant.Model
		}
	}
	if newest != "" && newest != "-" {
		return newest
	}
	return requested
}

// Due reports whether a threshold compaction should run before this step
// (section 08 §2).
func (c *LoopCompactor) Due(in StepInput) bool {
	if in.Path == nil {
		return false
	}
	model := compactionModel(in)
	return compactionDue(in.Path, model, pathModelWindow(in.Path, model))
}

// CompactIfNeeded runs a threshold compaction when Due.
func (c *LoopCompactor) CompactIfNeeded(ctx context.Context, in StepInput) (ModelStepUsage, error) {
	if !c.Due(in) {
		return ModelStepUsage{}, nil
	}
	return c.compact(ctx, in, CompactionTriggerThreshold)
}

// Compact compacts now: force=true is the overflow compaction after a length
// rejection (the loop attaches the rejection's pressure with
// withCompactionPressure), force=false a threshold compaction without the
// Due check.
func (c *LoopCompactor) Compact(ctx context.Context, in StepInput, force bool) (ModelStepUsage, error) {
	if force {
		return c.compact(ctx, in, CompactionTriggerOverflow)
	}
	return c.compact(ctx, in, CompactionTriggerThreshold)
}

func (c *LoopCompactor) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *LoopCompactor) newID() string {
	if c.NewID != nil {
		return c.NewID()
	}
	return uuid.NewString()
}

func (c *LoopCompactor) validate(in StepInput) error {
	switch {
	case c == nil || c.Host == nil || c.Sessions == nil || c.Requests == nil:
		return fmt.Errorf("%w: the compactor needs a host, a session store and a request builder", ErrTurnHostConfiguration)
	case in.Run == nil || in.Step == nil || in.Path == nil || in.Path.Session == nil:
		return fmt.Errorf("%w: compaction needs the Run, its step and its session path", ErrTurnHostConfiguration)
	case strings.TrimSpace(in.WorkerID) == "":
		return fmt.Errorf("%w: compaction needs the Run lease owner", ErrTurnHostConfiguration)
	case in.Run.Budget != nil && c.Budget == nil:
		return fmt.Errorf("%w: a bounded Run needs a compaction budget", ErrTurnHostConfiguration)
	}
	return nil
}

// compactionWork is what one compaction sends: the summary requests in
// order (one for threshold, the chunks for overflow).
type compactionWork struct {
	trigger  string
	model    string
	window   ModelWindow
	pressure *HostedContextPressure
	estimate PathEstimate
	plan     compactionPlan
	step     ModelStepRequest
	chunks   [][]*SessionEntry // overflow only
}

func (c *LoopCompactor) compact(ctx context.Context, in StepInput, trigger string) (ModelStepUsage, error) {
	if err := c.validate(in); err != nil {
		return ModelStepUsage{}, err
	}
	work := compactionWork{trigger: trigger, model: compactionModel(in), pressure: compactionPressureFrom(ctx)}
	work.window = pathModelWindow(in.Path, work.model)
	if trigger == CompactionTriggerOverflow && !work.window.Known() && work.pressure != nil && work.pressure.LimitTokens > 0 {
		work.window = ModelWindow{InputLimitTokens: work.pressure.LimitTokens}
	}
	work.estimate = estimatePathTokens(in.Path, work.model)
	plan, err := planCompaction(in.Path.Entries, compactionKeepFor(trigger, work.window))
	if err != nil {
		if errors.Is(err, errCompactionImpossible) {
			if trigger == CompactionTriggerThreshold {
				// Nothing to compact yet; the length-rejection path handles it.
				return ModelStepUsage{}, nil
			}
			return ModelStepUsage{}, compactionFailure(TurnHostContextExceeded)
		}
		return ModelStepUsage{}, err
	}
	work.plan = plan
	if trigger == CompactionTriggerOverflow {
		pressure := work.pressure
		if pressure == nil {
			pressure = &HostedContextPressure{EstimatedTokens: work.estimate.Tokens}
		}
		previous := int64(0)
		if in.Path.Compaction != nil {
			previous = compactionEntryTokens(in.Path.Compaction)
		}
		chunks, err := compactionChunks(plan.Summarized, previous, compactionChunkBudget(work.window, pressure))
		if err != nil {
			if errors.Is(err, errCompactionImpossible) {
				return ModelStepUsage{}, compactionFailure(TurnHostContextExceeded)
			}
			return ModelStepUsage{}, err
		}
		work.chunks = chunks
	}
	step, err := c.Requests.PrepareModelStep(ctx, in)
	if err != nil {
		return ModelStepUsage{}, err
	}
	work.step = step

	run := in.Run
	reservationID := compactionBudgetReservationID(in.Step.ID)
	if run.Budget != nil {
		reserved, err := c.Budget.ReserveCompaction(ctx, run, in.WorkerID, reservationID, compactionReservation(work))
		if errors.Is(err, ErrBudgetExhausted) {
			return ModelStepUsage{}, compactionBudgetFailure()
		}
		if err != nil {
			return ModelStepUsage{}, err
		}
		run = reserved
		*in.Run = *cloneAgentRun(reserved)
	}

	usage, err := c.summarizeAndAppend(ctx, in, work)
	if in.Run.Budget != nil {
		settled, settleErr := c.Budget.SettleCompaction(ctx, run, in.WorkerID, reservationID, compactionBudgetUsage(usage))
		if settleErr == nil {
			*in.Run = *cloneAgentRun(settled)
		} else if err == nil {
			err = settleErr
		}
	}
	return usage, err
}

// compactionReservation is the budget a compaction may use: its input
// estimate and a full summary output per planned call.
func compactionReservation(work compactionWork) BudgetUsage {
	if work.trigger != CompactionTriggerOverflow {
		return BudgetUsage{InputTokens: work.estimate.Tokens, OutputTokens: compactionSummaryMaxOutputTokens}
	}
	var input int64
	for _, chunk := range work.chunks {
		input += defaultPrefixTokens + compactionSpanTokens(chunk)
	}
	return BudgetUsage{InputTokens: input, OutputTokens: compactionSummaryMaxOutputTokens * int64(len(work.chunks))}
}

// compactionSummary is the outcome of the summary calls.
type compactionSummary struct {
	text     string
	provider string
	model    string
}

func (c *LoopCompactor) summarizeAndAppend(ctx context.Context, in StepInput, work compactionWork) (ModelStepUsage, error) {
	var (
		summary compactionSummary
		usage   ModelStepUsage
		err     error
	)
	if work.trigger == CompactionTriggerOverflow {
		summary, usage, err = c.summarizeOverflow(ctx, in, work)
	} else {
		summary, usage, err = c.summarize(ctx, compactionSummaryRequest(work.step, work.step.Entries))
		if isContextExceeded(err) {
			// The full path does not fit even for the summary: skip, and let
			// the step's own length rejection take the overflow path.
			return usage, nil
		}
	}
	if err != nil {
		return usage, err
	}
	return usage, c.appendCompaction(ctx, in, work, summary, usage)
}

// summarizeOverflow folds the summarized span chunk by chunk: chunk 0 after
// the previous compaction, chunk k after a synthetic compaction view of
// summary k−1. A chunk the host rejects for length is halved once.
func (c *LoopCompactor) summarizeOverflow(ctx context.Context, in StepInput, work compactionWork) (compactionSummary, ModelStepUsage, error) {
	views := make(map[string]SessionEntryView, len(work.step.Entries))
	for _, view := range work.step.Entries {
		views[view.ID] = view
	}
	type pendingChunk struct {
		entries []*SessionEntry
		split   bool
	}
	queue := make([]pendingChunk, 0, len(work.chunks))
	for _, chunk := range work.chunks {
		queue = append(queue, pendingChunk{entries: chunk})
	}
	var (
		summary compactionSummary
		usage   ModelStepUsage
		calls   int
	)
	for len(queue) > 0 {
		chunk := queue[0]
		queue = queue[1:]
		if calls+1+len(queue) > compactionMaxFolds {
			return summary, usage, compactionFailure(TurnHostContextExceeded)
		}
		chunkViews := compactionChunkViews(views, chunk.entries)
		if len(chunkViews) == 0 {
			// The renderer sent none of this chunk: nothing to fold in, so
			// the current summary stands.
			continue
		}
		entries := make([]SessionEntryView, 0, len(chunkViews)+1)
		if calls == 0 {
			if previous := previousCompactionView(in.Path, views); previous != nil {
				entries = append(entries, *previous)
			}
		} else {
			entries = append(entries, compactionFoldView(in.Step.ID, calls, summary.text, chunkViews))
		}
		entries = append(entries, chunkViews...)
		result, callUsage, err := c.summarize(ctx, compactionSummaryRequest(work.step, entries))
		usage = addModelStepUsage(usage, callUsage)
		if isContextExceeded(err) {
			if chunk.split {
				return summary, usage, compactionFailure(TurnHostContextExceeded)
			}
			first, second, ok := splitCompactionChunk(chunk.entries)
			if !ok {
				return summary, usage, compactionFailure(TurnHostContextExceeded)
			}
			queue = append([]pendingChunk{{entries: first, split: true}, {entries: second, split: true}}, queue...)
			continue
		}
		if err != nil {
			return summary, usage, err
		}
		summary = result
		calls++
	}
	if calls == 0 {
		return summary, usage, compactionFailure(TurnHostContextExceeded)
	}
	return summary, usage, nil
}

// compactionChunkViews picks the step request's views of chunk's entries, in
// chunk order. Entries the renderer did not send are skipped.
func compactionChunkViews(views map[string]SessionEntryView, chunk []*SessionEntry) []SessionEntryView {
	out := make([]SessionEntryView, 0, len(chunk))
	for _, entry := range chunk {
		if view, ok := views[entry.ID]; ok {
			out = append(out, view)
		}
	}
	return out
}

// previousCompactionView is the view of the path's latest compaction as the
// step sends it, or one built from the entry when the request lacks it.
func previousCompactionView(path *AgentSessionPath, views map[string]SessionEntryView) *SessionEntryView {
	if path == nil || path.Compaction == nil || path.Compaction.Compaction == nil {
		return nil
	}
	if view, ok := views[path.Compaction.ID]; ok {
		return &view
	}
	return &SessionEntryView{
		ID: path.Compaction.ID, Sequence: path.Compaction.Sequence, Type: SessionEntryCompaction, CreatedAt: path.Compaction.CreatedAt,
		Compaction: &CompactionEntryView{
			Summary:          CompactionSummaryText(path.Compaction.Compaction),
			FirstKeptEntryID: path.Compaction.Compaction.FirstKeptEntryID,
			TokensBefore:     path.Compaction.Compaction.TokensBefore,
		},
	}
}

// compactionFoldView is the synthetic, never persisted compaction view that
// carries summary k−1 into chunk k. It keeps the whole chunk as its "kept
// tail" (sequence after the chunk, first kept = the chunk's first entry), so
// the request validates whatever sequences the chunk has.
func compactionFoldView(stepID string, fold int, summary string, chunk []SessionEntryView) SessionEntryView {
	last := chunk[len(chunk)-1]
	return SessionEntryView{
		ID: fmt.Sprintf("compaction-fold:%s:%d", stepID, fold), Sequence: last.Sequence + 1,
		Type: SessionEntryCompaction, CreatedAt: last.CreatedAt,
		Compaction: &CompactionEntryView{
			Summary:          CompactionSummaryText(&CompactionEntry{Summary: summary}),
			FirstKeptEntryID: chunk[0].ID,
		},
	}
}

// compactionSummaryRequest turns the step's request into a summary request
// over entries: same identity, System, Tools and Stream.
func compactionSummaryRequest(step ModelStepRequest, entries []SessionEntryView) ModelStepRequest {
	req := step
	req.Purpose = ModelStepPurposeSummary
	req.Entries = entries
	req.SummaryInstruction = compactionSummaryInstruction
	req.MaxOutputTokens = compactionSummaryMaxOutputTokens
	req.Stream.AnswerPreview = false
	return req
}

// summarize runs one summary call, retried once identically when the host
// answers summary_failed or the summary is unusable. A second failure is
// compaction_failed. Length rejections and host errors return unchanged.
func (c *LoopCompactor) summarize(ctx context.Context, req ModelStepRequest) (compactionSummary, ModelStepUsage, error) {
	var usage ModelStepUsage
	if len(req.Entries) < compactionSummaryMinimumEntries {
		// A summary request needs two entries (section 06); one entry alone
		// that fills the window cannot be summarized, so it is a length
		// failure, not an invalid request.
		return compactionSummary{}, usage, &TurnHostFailure{
			Code:    TurnHostContextExceeded,
			Message: "The context is too large to summarize.",
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := c.Host.ExecuteModelStep(ctx, req)
		if err != nil {
			var failure *TurnHostFailure
			if errors.As(err, &failure) && failure.Code == modelStepSummaryFailed {
				continue
			}
			return compactionSummary{}, usage, err
		}
		usage = addModelStepUsage(usage, result.Assistant.Usage)
		text := strings.TrimSpace(result.Assistant.Text)
		if text == "" || len(text) > MaxCompactionSummaryBytes || !utf8.ValidString(text) || len(result.Assistant.ToolCalls) > 0 {
			continue
		}
		provider := string(result.Model.Provider)
		if provider == "" {
			provider = string(result.Assistant.Provider)
		}
		model := result.Model.Model
		if model == "" {
			model = result.Assistant.Model
		}
		return compactionSummary{text: text, provider: provider, model: model}, usage, nil
	}
	return compactionSummary{}, usage, compactionFailure(RunFailureCodeCompactionFailed)
}

// compactionSummaryMinimumEntries is the fewest entries a summary request
// may carry (ModelStepRequest.Validate).
const compactionSummaryMinimumEntries = 2

func isContextExceeded(err error) bool {
	var failure *TurnHostFailure
	return errors.As(err, &failure) && failure.Code == TurnHostContextExceeded
}

// appendCompaction appends the compaction entry and its session.compacted
// event in one lease-fenced store call.
func (c *LoopCompactor) appendCompaction(ctx context.Context, in StepInput, work compactionWork, summary compactionSummary, usage ModelStepUsage) error {
	now := c.now()
	var previous *CompactionEntry
	if in.Path.Compaction != nil {
		previous = in.Path.Compaction.Compaction
	}
	sessionID := in.Path.Session.ID
	entry := &SessionEntry{
		ID: c.newID(), Scope: in.Run.Scope, SessionID: sessionID, RunID: in.Run.ID,
		StepID: in.Step.ID, Step: in.Step.Sequence, Type: SessionEntryCompaction, CreatedAt: now,
		Compaction: &CompactionEntry{
			Summary:          summary.text,
			FirstKeptEntryID: work.plan.FirstKeptEntryID,
			TokensBefore:     work.estimate.Tokens,
			ActionCallIDs:    compactionActionCallIDs(previous, work.plan.Summarized),
			Provider:         summary.provider,
			Model:            summary.model,
			Trigger:          work.trigger,
			Usage:            usage,
		},
	}
	entry.Tokens = estimateEntryTokens(entry)
	tokensAfter := entry.Tokens + compactionSpanTokens(work.plan.Kept) + defaultPrefixTokens
	event := &ActivityEvent{
		ID: c.newID(), Scope: in.Run.Scope, EventType: compactionEventType, Severity: ActivitySeverityInfo,
		AgentID: in.Run.AssignedAgentID, ObjectiveID: in.Run.ObjectiveID, TeamID: teamIDForRun(in.Run),
		RunID: in.Run.ID, TurnID: in.Step.ID, Actor: ActivityActor{Type: "worker", ID: in.WorkerID},
		Summary: "Summarized earlier context", Visibility: ActivityVisibilityScope, CreatedAt: now,
		Payload: map[string]interface{}{
			"sessionId":        sessionID,
			"entryId":          entry.ID,
			"firstKeptEntryId": work.plan.FirstKeptEntryID,
			"trigger":          work.trigger,
			"tokensBefore":     work.estimate.Tokens,
			"tokensAfter":      tokensAfter,
		},
	}
	_, err := c.Sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: in.Run.Scope, SessionID: sessionID, RunID: in.Run.ID,
		Lease:   &AgentRunLeaseGuard{WorkerID: in.WorkerID, Now: now},
		Entries: []*SessionEntry{entry}, Events: []*ActivityEvent{event}, Now: now,
	})
	return err
}
