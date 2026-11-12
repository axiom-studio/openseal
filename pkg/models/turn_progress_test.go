package models

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTurnProgressBoundsTextAndHonorsCancellation(t *testing.T) {
	seen := ""
	ctx, cancel := context.WithCancel(WithTurnProgress(t.Context(), func(_ context.Context, s string) error { seen = s; return nil }))
	if err := ReportTurnProgress(ctx, strings.Repeat("界", 2000)); err != nil {
		t.Fatal(err)
	}
	if len(seen) > 4096 || !utf8.ValidString(seen) {
		t.Fatal("summary bounds invalid")
	}
	before := seen
	cancel()
	if err := ReportTurnProgress(ctx, "late"); !errors.Is(err, context.Canceled) || seen != before {
		t.Fatalf("late summary: %v", err)
	}
}
