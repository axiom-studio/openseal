package runtime

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// RoutineEventIndexRebuildInterval is how often the dispatcher loop rebuilds
// the matcher index from the store, as a backstop for lost change signals.
const RoutineEventIndexRebuildInterval = 15 * time.Minute

type routineEventIndexKey struct {
	kind      RoutineEventKind
	sourceRef string
}

type routineEventScopeKeys map[routineEventIndexKey]struct{}

// routineEventIndex is one immutable snapshot of the (scope, kind, SourceRef)
// keys that have at least one active event routine. Readers load it through
// an atomic pointer and never lock; writers build a new snapshot.
type routineEventIndex struct {
	scopes map[Scope]routineEventScopeKeys
}

func (i *routineEventIndex) has(scope Scope, kind RoutineEventKind, sourceRef string) bool {
	if i == nil {
		return false
	}
	keys := i.scopes[scope]
	if keys == nil {
		return false
	}
	_, ok := keys[routineEventIndexKey{kind: kind, sourceRef: sourceRef}]
	return ok
}

// routineEventIndexHolder owns the snapshot pointer. Writers are serialized,
// and each one reads the store and swaps the snapshot under the same lock, so
// an older read can never replace a newer one.
type routineEventIndexHolder struct {
	current atomic.Pointer[routineEventIndex]
	write   sync.Mutex
}

func (h *routineEventIndexHolder) listening(scope Scope, kind RoutineEventKind, sourceRef string) bool {
	return h.current.Load().has(scope, kind, sourceRef)
}

// refreshScope replaces one scope's keys, copying only the outer map.
func (h *routineEventIndexHolder) refreshScope(ctx context.Context, store RoutineEventStore, scope Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	h.write.Lock()
	defer h.write.Unlock()
	keys, err := store.ListRoutineEventSourceKeysInScope(ctx, scope)
	if err != nil {
		return err
	}
	previous := h.current.Load()
	next := &routineEventIndex{scopes: make(map[Scope]routineEventScopeKeys, 1)}
	if previous != nil {
		for existing, values := range previous.scopes {
			if existing != scope {
				next.scopes[existing] = values
			}
		}
	}
	if len(keys) > 0 {
		values := make(routineEventScopeKeys, len(keys))
		for _, key := range keys {
			values[routineEventIndexKey{kind: key.Kind, sourceRef: key.SourceRef}] = struct{}{}
		}
		next.scopes[scope] = values
	}
	h.current.Store(next)
	return nil
}

// rebuild reads every active event routine across scopes and replaces the
// whole snapshot.
func (h *routineEventIndexHolder) rebuild(ctx context.Context, store RoutineEventStore) error {
	h.write.Lock()
	defer h.write.Unlock()
	next := &routineEventIndex{scopes: make(map[Scope]routineEventScopeKeys)}
	after := ""
	for {
		page, cursor, err := store.ListRoutineEventSubscriptions(ctx, nil, after, routineEventSubscriptionPageLimit)
		if err != nil {
			return err
		}
		for _, subscription := range page {
			values := next.scopes[subscription.Scope]
			if values == nil {
				values = make(routineEventScopeKeys)
				next.scopes[subscription.Scope] = values
			}
			values[routineEventIndexKey{kind: subscription.Kind, sourceRef: subscription.SourceRef}] = struct{}{}
		}
		if cursor == "" || len(page) == 0 {
			break
		}
		after = cursor
	}
	h.current.Store(next)
	return nil
}

// routineActivationObserver is called after a committed activation write
// with the activation before (nil on create) and after the write.
type routineActivationObserver func(ctx context.Context, previous, next *RunbookActivation)

// Activation services are built on demand from a store in many places, so
// observers are registered per store instance rather than per service.
var routineActivationObservers struct {
	sync.RWMutex
	byStore map[interface{}]map[uint64]routineActivationObserver
	serial  uint64
}

func routineActivationObserverKey(store interface{}) (interface{}, bool) {
	if store == nil || !reflect.TypeOf(store).Comparable() {
		return nil, false
	}
	return store, true
}

// registerRoutineActivationObserver adds an observer for writes made through
// RunbookActivationService on this store. The returned func removes it.
func registerRoutineActivationObserver(store interface{}, observer routineActivationObserver) func() {
	key, ok := routineActivationObserverKey(store)
	if !ok || observer == nil {
		return func() {}
	}
	routineActivationObservers.Lock()
	defer routineActivationObservers.Unlock()
	if routineActivationObservers.byStore == nil {
		routineActivationObservers.byStore = make(map[interface{}]map[uint64]routineActivationObserver)
	}
	routineActivationObservers.serial++
	id := routineActivationObservers.serial
	if routineActivationObservers.byStore[key] == nil {
		routineActivationObservers.byStore[key] = make(map[uint64]routineActivationObserver)
	}
	routineActivationObservers.byStore[key][id] = observer
	return func() {
		routineActivationObservers.Lock()
		defer routineActivationObservers.Unlock()
		delete(routineActivationObservers.byStore[key], id)
		if len(routineActivationObservers.byStore[key]) == 0 {
			delete(routineActivationObservers.byStore, key)
		}
	}
}

// notifyRoutineActivationWrite runs the observers of a store after a
// committed activation write.
func notifyRoutineActivationWrite(ctx context.Context, store interface{}, previous, next *RunbookActivation) {
	key, ok := routineActivationObserverKey(store)
	if !ok {
		return
	}
	routineActivationObservers.RLock()
	observers := make([]routineActivationObserver, 0, len(routineActivationObservers.byStore[key]))
	for _, observer := range routineActivationObservers.byStore[key] {
		observers = append(observers, observer)
	}
	routineActivationObservers.RUnlock()
	for _, observer := range observers {
		observer(ctx, previous, next)
	}
}
