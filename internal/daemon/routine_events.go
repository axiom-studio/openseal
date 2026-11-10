package daemon

import (
	"context"
	"sync"

	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

// desktopRoutineEventSources is the daemon's host source catalog. The
// catalog carries only a host's own sources (Gmail and Calendar), and the
// daemon has no Google sign-in or sync loop, so it lists none. The sources
// the daemon does have come from its own store, not from this catalog: the
// Slack and Telegram connections and GitHub registrations saved there, and
// test events (send_test_event) for any routine. Webhooks need the platform
// hook key and public ingress, which the daemon has neither of.
type desktopRoutineEventSources struct{}

func (desktopRoutineEventSources) ListRoutineEventSources(context.Context, runtime.Scope, runtime.ObjectiveOwner, string) ([]runtime.RoutineEventSourceOption, error) {
	return nil, nil
}

// ConfigureRoutineEvents gives the kernel the daemon's event sources: no
// webhooks and the source catalog above. Call it before Start.
func ConfigureRoutineEvents(kernel *opensealkernel.Engine) {
	kernel.SetHooksAvailable(false)
	kernel.SetRoutineEventSourceCatalog(desktopRoutineEventSources{})
}

// StartRoutineEventDispatcher runs the kernel's routine event dispatcher
// until ctx ends or the returned stop is called; stop waits for the loop to
// exit. The daemon is a single process, so it always leads. Call it after
// Start.
func StartRoutineEventDispatcher(ctx context.Context, kernel *opensealkernel.Engine) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		kernel.RunRoutineEventDispatcher(ctx, runtime.RoutineEventDispatchInterval, nil)
	}()
	return func() {
		cancel()
		done.Wait()
	}
}
