package modelstep

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func planUser(id string, tokens int64) *models.SessionEntry {
	return &models.SessionEntry{ID: id, SessionID: "ses", Type: models.SessionEntryUser, Tokens: tokens,
		User: &models.UserEntry{Origin: models.UserEntryChannelMessage, Content: []models.ContentPart{{Type: models.ContentPartText, Text: "hi"}}}}
}

func planAssistant(id, step string, tokens int64) *models.SessionEntry {
	return &models.SessionEntry{ID: id, SessionID: "ses", StepID: step, Type: models.SessionEntryAssistant, Tokens: tokens,
		Assistant: &models.AssistantEntry{Model: "m", Text: "working"}}
}

func planTool(id, step string, tokens int64, state models.ToolEntryState) *models.SessionEntry {
	return &models.SessionEntry{ID: id, SessionID: "ses", StepID: step, Type: models.SessionEntryTool, State: state, Tokens: tokens,
		ToolCallID: "call-" + id, Tool: &models.ToolEntry{Name: "read"}}
}

func planNote(id string, tokens int64) *models.SessionEntry {
	return &models.SessionEntry{ID: id, SessionID: "ses", Type: models.SessionEntryNote, Tokens: tokens, Note: &models.NoteEntry{Text: "note"}}
}

func planIDs(entries []*models.SessionEntry) string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return strings.Join(ids, ",")
}

func planTestEntries() []*models.SessionEntry {
	return []*models.SessionEntry{
		planUser("u0", 5000),
		planAssistant("a1", "s1", 3000),
		planTool("t2", "s1", 3000, models.ToolEntryCompleted),
		planUser("u3", 2000),
		planAssistant("a4", "s2", 2000),
		planTool("t5", "s2", 2000, models.ToolEntryCompleted),
		planAssistant("a6", "s3", 1000),
	}
}

func TestPlanCompactionKeepsRecentTokens(t *testing.T) {
	entries := planTestEntries()
	plan, err := planCompaction(entries, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "a4" || planIDs(plan.Kept) != "a4,t5,a6" || planIDs(plan.Summarized) != "u0,a1,t2,u3" {
		t.Fatalf("plan = summarized %s kept %s first %s", planIDs(plan.Summarized), planIDs(plan.Kept), plan.FirstKeptEntryID)
	}
	if kept := compactionSpanTokens(plan.Kept); kept < 5000 {
		t.Fatalf("kept %d tokens, want at least 5000", kept)
	}
}

func TestPlanCompactionNeverStartsAtToolEntry(t *testing.T) {
	entries := planTestEntries()
	plan, err := planCompaction(entries, 2000) // reached at t5
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "a4" {
		t.Fatalf("first kept = %s, want a4 (the tool entry's assistant)", plan.FirstKeptEntryID)
	}
	for keep := int64(0); keep <= 20000; keep += 500 {
		plan, err := planCompaction(entries, keep)
		if errors.Is(err, errCompactionImpossible) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if first := plan.Kept[0]; !models.IsValidCompactionCut(first) {
			t.Fatalf("keep %d: first kept %s is a %s entry", keep, first.ID, first.Type)
		}
		if plan.Summarized[len(plan.Summarized)-1].Type == models.SessionEntryAssistant {
			t.Fatalf("keep %d: summarized span ends at an assistant entry without its results", keep)
		}
	}
}

func TestPlanCompactionFallsBackToLatestStep(t *testing.T) {
	entries := planTestEntries()
	plan, err := planCompaction(entries, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "a6" || planIDs(plan.Kept) != "a6" {
		t.Fatalf("kept %s, want only the latest step a6", planIDs(plan.Kept))
	}

	// The latest step is an assistant with its results: the whole step is kept.
	entries = append(planTestEntries(), planTool("t7", "s3", 500, models.ToolEntryCompleted))
	plan, err = planCompaction(entries, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if planIDs(plan.Kept) != "a6,t7" {
		t.Fatalf("kept %s, want a6,t7", planIDs(plan.Kept))
	}
}

func TestPlanCompactionImpossibleForSingleStep(t *testing.T) {
	single := []*models.SessionEntry{
		planAssistant("a0", "s1", 9000),
		planTool("t1", "s1", 9000, models.ToolEntryCompleted),
		planTool("t2", "s1", 9000, models.ToolEntryCompleted),
	}
	if _, err := planCompaction(single, 1000); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("err = %v, want errCompactionImpossible", err)
	}
	if _, err := planCompaction(nil, 1000); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("empty path: err = %v, want errCompactionImpossible", err)
	}

	// Under 4000 tokens before the cut: skipped, the length-rejection path handles it.
	small := []*models.SessionEntry{
		planUser("u0", 3000),
		planAssistant("a1", "s1", 20000),
		planTool("t2", "s1", 20000, models.ToolEntryCompleted),
	}
	if _, err := planCompaction(small, 1000); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("small span: err = %v, want errCompactionImpossible", err)
	}
}

func TestPlanCompactionChildMayKeepParentEntry(t *testing.T) {
	parentUser := planUser("p-u0", 6000)
	parentUser.SessionID = "parent"
	parentAssistant := planAssistant("p-a1", "ps1", 6000)
	parentAssistant.SessionID = "parent"
	childGoal := planUser("c-u2", 100)
	childGoal.SessionID = "child"
	childGoal.User.Origin = models.UserEntryGoal
	childAssistant := planAssistant("c-a3", "cs1", 100)
	childAssistant.SessionID = "child"

	plan, err := planCompaction([]*models.SessionEntry{parentUser, parentAssistant, childGoal, childAssistant}, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "p-a1" || plan.Kept[0].SessionID != "parent" {
		t.Fatalf("first kept = %s in %s, want the parent's p-a1", plan.FirstKeptEntryID, plan.Kept[0].SessionID)
	}
}

func TestPlanCompactionRespectsOpenToolEntries(t *testing.T) {
	// Another Run of the same session is waiting for an approval on t2: its
	// assistant a1 is the latest allowed first kept entry.
	entries := []*models.SessionEntry{
		planUser("u0", 5000),
		planAssistant("a1", "other-step", 3000),
		planTool("t2", "other-step", 3000, models.ToolEntryAwaitingApproval),
		planUser("u3", 2000),
		planAssistant("a4", "s2", 2000),
		planTool("t5", "s2", 2000, models.ToolEntryCompleted),
	}
	plan, err := planCompaction(entries, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "a1" {
		t.Fatalf("first kept = %s, want a1 (assistant of the open tool entry)", plan.FirstKeptEntryID)
	}
	for _, entry := range plan.Summarized {
		if entry.Type == models.SessionEntryTool && entry.State.Open() {
			t.Fatalf("summarized open tool entry %s", entry.ID)
		}
	}
	if !models.IsValidCompactionCutAt(entries, len(plan.Summarized)) {
		t.Fatal("store would reject the planned cut")
	}

	// The bound leaves fewer than 4000 tokens to summarize: skip.
	entries[0].Tokens = 1000
	if _, err := planCompaction(entries, 1000); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("err = %v, want errCompactionImpossible", err)
	}

	// An open tool entry whose assistant was compacted earlier bounds the cut at itself.
	orphan := []*models.SessionEntry{
		planUser("u0", 5000),
		planNote("n1", 100),
		planTool("t2", "gone", 100, models.ToolEntryRunning),
		planUser("u3", 100),
		planAssistant("a4", "s2", 100),
	}
	plan, err = planCompaction(orphan, 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FirstKeptEntryID != "n1" {
		t.Fatalf("first kept = %s, want n1 (before the open tool entry)", plan.FirstKeptEntryID)
	}
}

func TestCompactionKeepFor(t *testing.T) {
	window := ModelWindow{ContextTokens: 200000}
	if got := compactionKeepFor(models.CompactionTriggerThreshold, window); got != 15000 {
		t.Fatalf("threshold keep = %d, want 15000", got)
	}
	if got := compactionKeepFor(models.CompactionTriggerOverflow, window); got != 7500 {
		t.Fatalf("overflow keep = %d, want 7500", got)
	}
	if got := compactionKeepFor(models.CompactionTriggerOverflow, ModelWindow{}); got != 2048 {
		t.Fatalf("overflow keep with unknown window = %d, want 2048", got)
	}
}

func TestCompactionChunkBudget(t *testing.T) {
	cases := []struct {
		name     string
		window   ModelWindow
		pressure *HostedContextPressure
		want     int64
	}{
		{"input limit", ModelWindow{ContextTokens: 400000, InputLimitTokens: 272000}, nil, 272000 - 16384 - 16384 - 2048},
		{"context only", ModelWindow{ContextTokens: 200000}, nil, 200000 - 16384 - 16384 - 2048},
		{"pressure limit", ModelWindow{}, &HostedContextPressure{EstimatedTokens: 250000, LimitTokens: 198976}, 198976 - 16384 - 16384 - 2048},
		{"pressure estimate", ModelWindow{}, &HostedContextPressure{EstimatedTokens: 90000}, 45000},
		{"unknown", ModelWindow{}, nil, 0},
		{"tiny", ModelWindow{InputLimitTokens: 20000}, nil, 0},
	}
	for _, tc := range cases {
		if got := compactionChunkBudget(tc.window, tc.pressure); got != tc.want {
			t.Errorf("%s: budget = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// chunkTestSpan is n steps of user 1000 + assistant 2000 + two tools 1000 each.
func chunkTestSpan(n int) []*models.SessionEntry {
	var entries []*models.SessionEntry
	for i := 0; i < n; i++ {
		step := fmt.Sprintf("s%d", i)
		entries = append(entries,
			planUser(fmt.Sprintf("u%d", i), 1000),
			planAssistant(fmt.Sprintf("a%d", i), step, 2000),
			planTool(fmt.Sprintf("t%da", i), step, 1000, models.ToolEntryCompleted),
			planTool(fmt.Sprintf("t%db", i), step, 1000, models.ToolEntryError),
		)
	}
	return entries
}

func TestCompactionChunksEndAtValidCuts(t *testing.T) {
	span := chunkTestSpan(20) // 100000 tokens
	const budget, previous int64 = 40000, 3000
	chunks, err := compactionChunks(span, previous, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want several", len(chunks))
	}
	var joined []*models.SessionEntry
	for k, chunk := range chunks {
		if !models.IsValidCompactionCut(chunk[0]) {
			t.Fatalf("chunk %d starts at %s (%s)", k, chunk[0].ID, chunk[0].Type)
		}
		if last := chunk[len(chunk)-1]; last.Type == models.SessionEntryAssistant {
			t.Fatalf("chunk %d ends in assistant %s without its results", k, last.ID)
		}
		reserve := compactionFoldReserveTokens
		if k == 0 {
			reserve = previous
		}
		if tokens := compactionSpanTokens(chunk) + reserve; tokens > budget {
			t.Fatalf("chunk %d estimated %d tokens, over budget %d", k, tokens, budget)
		}
		joined = append(joined, chunk...)
	}
	if planIDs(joined) != planIDs(span) {
		t.Fatal("chunks do not cover the span in order")
	}

	// Everything fits: one chunk, no folding.
	chunks, err = compactionChunks(span[:8], 0, budget)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("small span: %d chunks, err %v; want 1", len(chunks), err)
	}

	// One step over the budget cannot be split.
	big := []*models.SessionEntry{planUser("u0", 100), planAssistant("a1", "s1", 30000), planTool("t2", "s1", 30000, models.ToolEntryCompleted)}
	if _, err := compactionChunks(big, 0, budget); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("oversized step: err = %v, want errCompactionImpossible", err)
	}

	// More than compactionMaxFolds calls.
	if _, err := compactionChunks(chunkTestSpan(100), 0, budget); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("too many folds: err = %v, want errCompactionImpossible", err)
	}
	if _, err := compactionChunks(span, 0, 0); !errors.Is(err, errCompactionImpossible) {
		t.Fatalf("zero budget: err = %v, want errCompactionImpossible", err)
	}
}

func TestSplitCompactionChunkHalvesAtValidCut(t *testing.T) {
	chunk := chunkTestSpan(4)
	first, second, ok := splitCompactionChunk(chunk)
	if !ok {
		t.Fatal("split failed")
	}
	if planIDs(first) != planIDs(chunk[:8]) || !models.IsValidCompactionCut(second[0]) {
		t.Fatalf("split = %s | %s", planIDs(first), planIDs(second))
	}
	single := []*models.SessionEntry{planAssistant("a0", "s0", 100), planTool("t1", "s0", 100, models.ToolEntryCompleted)}
	if _, _, ok := splitCompactionChunk(single); ok {
		t.Fatal("split one indivisible step")
	}
}

func TestCompactionActionCallIDsAreUnionOfPreviousAndSpan(t *testing.T) {
	span := chunkTestSpan(2)
	span[2].ActionCallID = "ac_3"
	span[3].ActionCallID = "ac_1" // already in the previous compaction
	span[6].ActionCallID = "ac_4"
	span[1].ActionCallID = "ac_ignored" // not a tool entry
	previous := &models.CompactionEntry{ActionCallIDs: []string{"ac_1", "ac_2"}}
	if got := strings.Join(compactionActionCallIDs(previous, span), ","); got != "ac_1,ac_2,ac_3,ac_4" {
		t.Fatalf("ids = %s", got)
	}

	many := &models.CompactionEntry{}
	for i := 0; i < compactionActionCallsKept; i++ {
		many.ActionCallIDs = append(many.ActionCallIDs, fmt.Sprintf("ac_old_%d", i))
	}
	ids := compactionActionCallIDs(many, span)
	if len(ids) != compactionActionCallsKept || ids[0] != "ac_old_3" || ids[len(ids)-1] != "ac_4" {
		t.Fatalf("capped ids: len %d first %s last %s", len(ids), ids[0], ids[len(ids)-1])
	}
}

func TestCompactionSummaryInstructionHeadings(t *testing.T) {
	headings := []string{"## Objective", "## Decisions", "## Done", "## Active", "## Blockers", "## Next", "## Files"}
	at := 0
	for _, heading := range headings {
		i := strings.Index(compactionSummaryInstruction[at:], "\n"+heading+"\n")
		if i < 0 {
			t.Fatalf("heading %q missing or out of order", heading)
		}
		at += i + 1
	}
	if !strings.HasPrefix(compactionSummaryInstruction, "Your context is being compacted.") ||
		!strings.HasSuffix(compactionSummaryInstruction, "Reply with the summary only.") {
		t.Fatal("instruction text changed")
	}
}
