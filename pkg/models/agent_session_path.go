package models

import (
	"fmt"
	"sort"
)

// BuildAgentSessionPath is the pure merge used by all backends. chain is root
// ... leaf; segments[i] holds entries of chain[i] (any order, may include
// entries beyond the segment bound and compaction entries; both are dropped).
// compaction is the latest compaction on the path, or nil to find it in the
// segments. Leaf bound = NextSequence-1; ancestor i bound = chain[i+1].ForkSequence.
func BuildAgentSessionPath(chain []*AgentSession, segments [][]*SessionEntry, compaction *SessionEntry) (*AgentSessionPath, error) {
	if len(chain) == 0 {
		return nil, ErrAgentSessionNotFound
	}
	if len(segments) != len(chain) {
		return nil, fmt.Errorf("%w: %d segments for %d sessions", ErrInvalidAgentSession, len(segments), len(chain))
	}
	if len(chain) > MaxAgentSessionDepth+1 {
		return nil, ErrAgentSessionForkTooDeep
	}
	bounds := make([]int64, len(chain))
	index := make(map[string]int, len(chain))
	for i, session := range chain {
		if session == nil {
			return nil, fmt.Errorf("%w: nil session in chain", ErrInvalidAgentSession)
		}
		if i == 0 && session.ParentSessionID != "" {
			return nil, fmt.Errorf("%w: chain does not start at the root", ErrInvalidAgentSession)
		}
		if i > 0 && session.ParentSessionID != chain[i-1].ID {
			return nil, fmt.Errorf("%w: session %s is not a child of %s", ErrInvalidAgentSession, session.ID, chain[i-1].ID)
		}
		index[session.ID] = i
		if i == len(chain)-1 {
			bounds[i] = session.NextSequence - 1
		} else {
			bounds[i] = chain[i+1].ForkSequence
		}
	}

	type position struct {
		segment  int
		sequence int64
	}
	bounded := make([][]*SessionEntry, len(chain))
	var latest *SessionEntry
	latestPos := position{segment: -1}
	positions := make(map[string]position)
	for i, segment := range segments {
		for _, entry := range segment {
			if entry == nil {
				continue
			}
			if entry.SessionID != chain[i].ID {
				return nil, fmt.Errorf("%w: entry %s is not in session %s", ErrInvalidSessionEntry, entry.ID, chain[i].ID)
			}
			if entry.Sequence < 1 || entry.Sequence > bounds[i] {
				continue
			}
			positions[entry.ID] = position{segment: i, sequence: entry.Sequence}
			if entry.Type == SessionEntryCompaction {
				if entry.Compaction == nil {
					return nil, fmt.Errorf("%w: compaction %s has no payload", ErrInvalidSessionEntry, entry.ID)
				}
				if latestPos.segment < i || (latestPos.segment == i && entry.Sequence > latestPos.sequence) {
					latest, latestPos = entry, position{segment: i, sequence: entry.Sequence}
				}
				continue
			}
			bounded[i] = append(bounded[i], entry)
		}
		sort.SliceStable(bounded[i], func(a, b int) bool { return bounded[i][a].Sequence < bounded[i][b].Sequence })
	}
	if compaction != nil {
		seg, ok := index[compaction.SessionID]
		if !ok || compaction.Type != SessionEntryCompaction || compaction.Compaction == nil ||
			compaction.Sequence < 1 || compaction.Sequence > bounds[seg] {
			return nil, fmt.Errorf("%w: compaction %s is not on the path", ErrInvalidSessionEntry, compaction.ID)
		}
		latest, latestPos = compaction, position{segment: seg, sequence: compaction.Sequence}
	}

	start := position{segment: 0, sequence: 1}
	if latest != nil {
		if kept := latest.Compaction.FirstKeptEntryID; kept == "" {
			start = position{segment: latestPos.segment, sequence: latestPos.sequence + 1}
		} else {
			pos, ok := positions[kept]
			if !ok {
				return nil, fmt.Errorf("%w: compaction %s keeps %s, which is not on the path", ErrInvalidSessionEntry, latest.ID, kept)
			}
			start = pos
		}
	}

	path := &AgentSessionPath{Session: chain[len(chain)-1], Chain: chain, Compaction: latest}
	if latest != nil {
		path.Tokens = latest.Tokens
	}
	for i := start.segment; i < len(chain); i++ {
		for _, entry := range bounded[i] {
			if i == start.segment && entry.Sequence < start.sequence {
				continue
			}
			path.Entries = append(path.Entries, entry)
			path.Tokens += entry.Tokens
		}
	}
	if len(path.Entries) > MaxAgentSessionPathEntries {
		return nil, ErrAgentSessionPathTooLarge
	}
	return path, nil
}
