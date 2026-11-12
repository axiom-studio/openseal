package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestPlanModelStepReservation(t *testing.T) {
	cases := []struct {
		name      string
		policy    BudgetPolicy
		remaining BudgetPolicy
		input     int64
		want      BudgetUsage
		dimension string
	}{
		{name: "unbounded tokens", policy: BudgetPolicy{MaxTurns: 3}, input: 900},
		{name: "input fits", policy: BudgetPolicy{MaxInputTokens: 1000}, remaining: BudgetPolicy{MaxInputTokens: 1000}, input: 900, want: BudgetUsage{InputTokens: 900}},
		{name: "input short", policy: BudgetPolicy{MaxInputTokens: 1000}, remaining: BudgetPolicy{MaxInputTokens: 899}, input: 900, dimension: "input_tokens"},
		{name: "total takes the rest", policy: BudgetPolicy{MaxTotalTokens: 2000}, remaining: BudgetPolicy{MaxTotalTokens: 1500}, input: 900, want: BudgetUsage{InputTokens: 900, OutputTokens: 600}},
		{name: "total short of minimum output", policy: BudgetPolicy{MaxTotalTokens: 2000}, remaining: BudgetPolicy{MaxTotalTokens: 900 + stepMinimumOutputTokens - 1}, input: 900, dimension: "total_tokens"},
		{name: "output caps total", policy: BudgetPolicy{MaxTotalTokens: 2000, MaxOutputTokens: 100}, remaining: BudgetPolicy{MaxTotalTokens: 1500, MaxOutputTokens: 100}, input: 900, want: BudgetUsage{InputTokens: 900, OutputTokens: 100}},
		{name: "output only", policy: BudgetPolicy{MaxOutputTokens: 500}, remaining: BudgetPolicy{MaxOutputTokens: 300}, input: 900, want: BudgetUsage{OutputTokens: 300}},
		{name: "output short", policy: BudgetPolicy{MaxOutputTokens: 500}, remaining: BudgetPolicy{MaxOutputTokens: stepMinimumOutputTokens - 1}, input: 900, dimension: "output_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planModelStepReservation(tc.policy, tc.remaining, tc.input)
			if tc.dimension != "" {
				var admission *BudgetAdmissionError
				if !errors.As(err, &admission) || !errors.Is(err, ErrBudgetExhausted) || admission.Admission.Dimension != tc.dimension ||
					admission.Admission.Reason != BudgetAdmissionHostedInput {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("reservation = %#v err=%v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestEstimateModelStepInputTokens(t *testing.T) {
	text := ModelStepRequest{
		APIVersion: ModelStepAPIVersion, StepID: "step",
		Entries: []SessionEntryView{{ID: "u", Type: SessionEntryUser, User: &UserEntryView{Content: []ContentPart{{Type: ContentPartText, Text: "hello"}}}}},
	}
	base, err := EstimateModelStepInputTokens(text)
	if err != nil || base <= stepProtocolInputReserveTokens {
		t.Fatalf("base estimate = %d err=%v", base, err)
	}

	image := text
	image.Entries = []SessionEntryView{text.Entries[0], {
		ID: "t", Type: SessionEntryTool, Tool: &ToolEntryView{CallID: "c", Name: "shot", Result: ToolResultView{Content: []ContentPart{
			{Type: ContentPartImage, MediaType: "image/png", DataBase64: strings.Repeat("A", 1<<20)},
		}}},
	}}
	withImage, err := EstimateModelStepInputTokens(image)
	if err != nil {
		t.Fatal(err)
	}
	if extra := withImage - base; extra < stepMediaReserveTokens || extra > stepMediaReserveTokens+200 {
		t.Fatalf("image estimate adds %d tokens; bytes must not be counted", extra)
	}
	if image.Entries[1].Tool.Result.Content[0].DataBase64 == "" {
		t.Fatal("estimate mutated the request")
	}

	plain, err := modelStepReservationInput(text, false)
	if err != nil || plain != base*stepMaximumProviderAttempts+stepRepairInputReserveTokens {
		t.Fatalf("reservation input = %d err=%v", plain, err)
	}
	due, err := modelStepReservationInput(text, true)
	if err != nil || due != 2*plain {
		t.Fatalf("compaction-due reservation input = %d, want %d", due, 2*plain)
	}
}
