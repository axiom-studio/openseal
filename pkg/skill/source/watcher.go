package source

import (
	"context"
	"errors"
	"sort"
	"time"
)

type Change struct {
	PreviousRevision string    `json:"previousRevision,omitempty"`
	Revision         string    `json:"revision"`
	Added            []string  `json:"added,omitempty"`
	Updated          []string  `json:"updated,omitempty"`
	Removed          []string  `json:"removed,omitempty"`
	Snapshot         *Snapshot `json:"snapshot"`
}

type Watcher struct {
	catalog  *Catalog
	roots    []Root
	interval time.Duration
}

func NewWatcher(catalog *Catalog, roots []Root, interval time.Duration) (*Watcher, error) {
	if catalog == nil || interval <= 0 {
		return nil, errors.New("skill source catalog and positive watch interval are required")
	}
	if _, err := normalizeRoots(roots); err != nil {
		return nil, err
	}
	return &Watcher{catalog: catalog, roots: append([]Root(nil), roots...), interval: interval}, nil
}

// Watch emits an initial effective snapshot and subsequent changes only when
// the effective source revision changes. Shadowed edits do not churn sessions.
func (w *Watcher) Watch(ctx context.Context) <-chan Change {
	changes := make(chan Change, 1)
	go func() {
		defer close(changes)
		var previous *Snapshot
		var pending *Snapshot
		scan := func() bool {
			current, err := w.catalog.Discover(ctx, w.roots)
			if err != nil {
				return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
			}
			if previous == nil {
				change := diffSnapshots(nil, current)
				select {
				case changes <- change:
				case <-ctx.Done():
					return false
				}
				previous = current
				return true
			}
			if previous.Revision == current.Revision {
				pending = nil
				return true
			}
			// Require the same new effective revision on two consecutive scans.
			// Editors and installers often replace SKILL.md in multiple writes;
			// this prevents transient partial content from churning sessions.
			if pending == nil || pending.Revision != current.Revision {
				pending = current
				return true
			}
			change := diffSnapshots(previous, current)
			select {
			case changes <- change:
			case <-ctx.Done():
				return false
			}
			previous = current
			pending = nil
			return true
		}
		if !scan() {
			return
		}
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !scan() {
					return
				}
			}
		}
	}()
	return changes
}

func diffSnapshots(previous, current *Snapshot) Change {
	change := Change{Revision: current.Revision, Snapshot: current}
	if previous != nil {
		change.PreviousRevision = previous.Revision
	}
	before := effectiveIdentities(previous)
	after := effectiveIdentities(current)
	for name, identity := range after {
		old, exists := before[name]
		if !exists {
			change.Added = append(change.Added, name)
		} else if old != identity {
			change.Updated = append(change.Updated, name)
		}
	}
	for name := range before {
		if _, exists := after[name]; !exists {
			change.Removed = append(change.Removed, name)
		}
	}
	sort.Strings(change.Added)
	sort.Strings(change.Updated)
	sort.Strings(change.Removed)
	return change
}

func effectiveIdentities(snapshot *Snapshot) map[string]string {
	result := make(map[string]string)
	if snapshot == nil {
		return result
	}
	for _, candidate := range snapshot.Effective {
		result[candidate.Name] = candidate.Digest + "\x00" + candidate.Version + "\x00" + candidate.RootID + "\x00" + candidate.Directory
	}
	return result
}
