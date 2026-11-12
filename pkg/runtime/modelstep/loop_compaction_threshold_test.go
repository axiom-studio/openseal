package modelstep

import "testing"

func TestLoopCompactionThreshold(t *testing.T) {
	for _, tc := range []struct{ window, threshold int64 }{
		{64_000, 48_000},     // 15 % = 9 600 is below the 16k floor
		{106_667, 90_667},    // 15 % = 16 000
		{200_000, 170_000},   // 30 000
		{1_000_000, 850_000}, // 150 000
	} {
		if got := compactionThreshold(ModelWindow{ContextTokens: tc.window}); got != tc.threshold {
			t.Errorf("compactionThreshold(%d) = %d, want %d", tc.window, got, tc.threshold)
		}
	}
}
