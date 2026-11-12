package runworker

import (
	"testing"
	"time"
)

func TestTerminalRunSweepProgressDoesNotRepeatCompletedSweeps(t *testing.T) {
	progress := newTerminalRunSweepProgress()
	now := time.Now()
	if offset, due := progress.next("a", now); offset != 0 || !due {
		t.Fatalf("new sweep = %d %t", offset, due)
	}
	progress.advance("a", 20)
	if offset, due := progress.next("a", now); offset != 20 || !due {
		t.Fatalf("resumed sweep = %d %t", offset, due)
	}
	progress.finish("a", now)
	if _, due := progress.next("a", now.Add(time.Hour)); due {
		t.Fatal("completed sweep restarted within its repeat interval")
	}
	if offset, due := progress.next("a", now.Add(terminalRunSweepRepeat)); offset != 0 || !due {
		t.Fatalf("sweep did not restart after its repeat interval: %d %t", offset, due)
	}
}
