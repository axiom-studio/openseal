package runloop

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

// stepLease serializes every write to the running step row in this process:
// the heartbeat, RecordStepModel, FinishTurn and ReleaseTurn. Each takes mu,
// uses turn.Revision as ExpectedRevision and replaces turn with the result.
type stepLease struct {
	mu   sync.Mutex
	turn *models.AgentTurn
	lost atomic.Bool

	stopOnce sync.Once
	stop     context.CancelFunc
	done     chan struct{}
}

func (s *stepLease) get() *models.AgentTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// write runs fn on the latest row under mu and stores its result.
func (s *stepLease) write(fn func(turn *models.AgentTurn) (*models.AgentTurn, error)) (*models.AgentTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := fn(s.turn)
	if err != nil {
		return nil, err
	}
	if next != nil {
		s.turn = next
	}
	return s.turn, nil
}

// startHeartbeat renews the step lease every duration/3. A failed renewal
// marks the lease lost and cancels the step context.
func (s *stepLease) startHeartbeat(ctx context.Context, cancelStep context.CancelFunc, turns *activity.AgentTurnService, duration time.Duration, workerID string) {
	hbCtx, stop := context.WithCancel(ctx)
	s.stop = stop
	s.done = make(chan struct{})
	interval := duration / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				_, err := s.write(func(turn *models.AgentTurn) (*models.AgentTurn, error) {
					return turns.RenewTurn(hbCtx, turn.Scope, turn.ID, workerID, duration)
				})
				if err != nil {
					if hbCtx.Err() != nil {
						return
					}
					s.lost.Store(true)
					cancelStep()
					return
				}
			}
		}
	}()
}

func (s *stepLease) stopHeartbeat() {
	s.stopOnce.Do(func() {
		if s.stop != nil {
			s.stop()
			<-s.done
		}
	})
}
