package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestTurnActionIdempotencyRefreshesReadsAcrossTurns(t *testing.T) {
	const modelKey = "same-model-key"
	first := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-before-navigation")
	retry := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-before-navigation")
	refresh := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-after-navigation")
	if first != retry || first == refresh {
		t.Fatalf("read key must replay within one Turn and refresh in a later Turn: %q %q %q", first, retry, refresh)
	}
	if turnActionIdempotencyKey(modelKey, capability.SideEffectNone, "turn-after-navigation") != refresh {
		t.Fatal("side-effect-free observations must follow read idempotency")
	}
	if turnActionIdempotencyKey(modelKey, capability.SideEffectWrite, "turn-after-navigation") != modelKey ||
		turnActionIdempotencyKey(modelKey, capability.SideEffectExternal, "turn-after-navigation") != modelKey {
		t.Fatal("mutating actions lost their stable semantic idempotency key")
	}
}
