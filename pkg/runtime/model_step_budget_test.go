package runtime

import (
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestPlanModelStepReservation(t *testing.T) {
	cases := []struct {
		name      string
		policy    models.BudgetPolicy
		remaining models.BudgetPolicy
		input     int64
		want      models.BudgetUsage
		dimension string
	}{
		{name: "unbounded tokens", policy: models.BudgetPolicy{MaxTurns: 3}, input: 900},
		{name: "input fits", policy: models.BudgetPolicy{MaxInputTokens: 1000}, remaining: models.BudgetPolicy{MaxInputTokens: 1000}, input: 900, want: models.BudgetUsage{InputTokens: 900}},
		{name: "input short", policy: models.BudgetPolicy{MaxInputTokens: 1000}, remaining: models.BudgetPolicy{MaxInputTokens: 899}, input: 900, dimension: "input_tokens"},
		{name: "total takes the rest", policy: models.BudgetPolicy{MaxTotalTokens: 2000}, remaining: models.BudgetPolicy{MaxTotalTokens: 1500}, input: 900, want: models.BudgetUsage{InputTokens: 900, OutputTokens: 600}},
		{name: "total short of minimum output", policy: models.BudgetPolicy{MaxTotalTokens: 2000}, remaining: models.BudgetPolicy{MaxTotalTokens: 900 + stepMinimumOutputTokens - 1}, input: 900, dimension: "total_tokens"},
		{name: "output caps total", policy: models.BudgetPolicy{MaxTotalTokens: 2000, MaxOutputTokens: 100}, remaining: models.BudgetPolicy{MaxTotalTokens: 1500, MaxOutputTokens: 100}, input: 900, want: models.BudgetUsage{InputTokens: 900, OutputTokens: 100}},
		{name: "output only", policy: models.BudgetPolicy{MaxOutputTokens: 500}, remaining: models.BudgetPolicy{MaxOutputTokens: 300}, input: 900, want: models.BudgetUsage{OutputTokens: 300}},
		{name: "output short", policy: models.BudgetPolicy{MaxOutputTokens: 500}, remaining: models.BudgetPolicy{MaxOutputTokens: stepMinimumOutputTokens - 1}, input: 900, dimension: "output_tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := planModelStepReservation(tc.policy, tc.remaining, tc.input)
			if tc.dimension != "" {
				var admission *models.BudgetAdmissionError
				if !errors.As(err, &admission) || !errors.Is(err, models.ErrBudgetExhausted) || admission.Admission.Dimension != tc.dimension ||
					admission.Admission.Reason != models.BudgetAdmissionHostedInput {
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
	text := models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion, StepID: "step",
		Entries: []models.SessionEntryView{{ID: "u", Type: models.SessionEntryUser, User: &models.UserEntryView{Content: []models.ContentPart{{Type: models.ContentPartText, Text: "hello"}}}}},
	}
	base, err := EstimateModelStepInputTokens(text)
	if err != nil || base <= stepProtocolInputReserveTokens {
		t.Fatalf("base estimate = %d err=%v", base, err)
	}

	image := text
	image.Entries = []models.SessionEntryView{text.Entries[0], {
		ID: "t", Type: models.SessionEntryTool, Tool: &models.ToolEntryView{CallID: "c", Name: "shot", Result: models.ToolResultView{Content: []models.ContentPart{
			{Type: models.ContentPartImage, MediaType: "image/png", DataBase64: strings.Repeat("A", 1<<20)},
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
