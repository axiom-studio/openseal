package runtime

import "context"

// StartsThread ancestry is bounded separately from ordinary reply history.
// Existing messages retain their direct-parent audience contract; a new root
// additionally preserves the restrictions of every thread-start anchor it
// follows. Long transcripts therefore never require walking every utterance.
const maximumChannelThreadStartDepth = 16
const maximumChannelThreadStartParentReads = MaximumChannelMessageBatchSize * maximumChannelThreadStartDepth

type channelMessageParentLoader func(context.Context, []string) (map[string]*ChannelMessage, error)

type channelThreadStartResult struct {
	allowed  bool
	height   int
	complete bool
}

type channelThreadStartProof struct {
	scope          Scope
	conversationID string
	parents        map[string]*ChannelMessage
	load           channelMessageParentLoader
	reads          int
	results        map[string]channelThreadStartResult
}

func newChannelThreadStartProof(scope Scope, conversationID string, known map[string]*ChannelMessage, load channelMessageParentLoader) *channelThreadStartProof {
	proof := &channelThreadStartProof{scope: scope, conversationID: conversationID, parents: make(map[string]*ChannelMessage), load: load}
	for id, message := range known {
		if message != nil && message.ID == id && message.Scope == scope && message.ConversationID == conversationID {
			proof.parents[id] = message
		}
	}
	return proof
}

func (p *channelThreadStartProof) loadParents(ctx context.Context, ids []string) error {
	missing := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, exists := p.parents[id]; exists {
			continue
		}
		// Missing and over-bound parents stay explicitly unknown. Their roots
		// fail closed without expanding a caller-controlled graph indefinitely.
		p.parents[id] = nil
		if p.reads < maximumChannelThreadStartParentReads {
			missing = append(missing, id)
			p.reads++
		}
	}
	if len(missing) == 0 {
		return nil
	}
	loaded, err := p.load(ctx, missing)
	if err != nil {
		return err
	}
	for _, id := range missing {
		message := loaded[id]
		if message != nil && message.ID == id && message.Scope == p.scope && message.ConversationID == p.conversationID {
			p.parents[id] = message
		}
	}
	return nil
}

func directChannelMessageParents(message *ChannelMessage) []string {
	if message == nil {
		return nil
	}
	ids := make([]string, 0, 2)
	for _, id := range []string{message.ThreadRootID, message.ReplyToMessageID} {
		if id != "" && id != message.ID && (len(ids) == 0 || ids[0] != id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// Parent reads are staged across the whole page, not issued once per reply.
// Only a new StartsThread anchor expands ancestry; ordinary direct parents do
// not recursively expand older conversation history.
func (p *channelThreadStartProof) preload(ctx context.Context, anchors []string, viewer *ConversationViewer) error {
	visited := make(map[string]bool)
	for depth := 0; depth < maximumChannelThreadStartDepth && len(anchors) > 0; depth++ {
		current := make([]string, 0, len(anchors))
		parentIDs := make([]string, 0, len(anchors))
		for _, id := range anchors {
			if visited[id] {
				continue
			}
			visited[id] = true
			anchor := p.parents[id]
			if anchor == nil || !anchor.StartsThread || viewer != nil && !CanViewChannelMessage(anchor, *viewer) {
				continue
			}
			current = append(current, id)
			parentIDs = append(parentIDs, anchor.ReplyToMessageID)
		}
		if err := p.loadParents(ctx, parentIDs); err != nil {
			return err
		}
		ordinaryIDs := make([]string, 0, len(current)*2)
		for _, id := range current {
			parent := p.parents[p.parents[id].ReplyToMessageID]
			if parent != nil && !parent.StartsThread && (viewer == nil || CanViewChannelMessage(parent, *viewer)) {
				ordinaryIDs = append(ordinaryIDs, directChannelMessageParents(parent)...)
			}
		}
		if err := p.loadParents(ctx, ordinaryIDs); err != nil {
			return err
		}
		anchors = nil
		for _, id := range current {
			parent := p.parents[p.parents[id].ReplyToMessageID]
			if parent == nil || viewer != nil && !CanViewChannelMessage(parent, *viewer) {
				continue
			}
			if parent.StartsThread {
				anchors = append(anchors, parent.ID)
				continue
			}
			for _, linked := range directChannelMessageParents(parent) {
				if ancestor := p.parents[linked]; ancestor != nil && ancestor.StartsThread {
					anchors = append(anchors, ancestor.ID)
				}
			}
		}
	}
	return nil
}

func (p *channelThreadStartProof) visit(id string, depth int, viewer *ConversationViewer, active map[string]bool) channelThreadStartResult {
	if active[id] {
		return channelThreadStartResult{complete: true}
	}
	if cached, ok := p.results[id]; ok {
		if depth+cached.height > maximumChannelThreadStartDepth {
			return channelThreadStartResult{height: cached.height}
		}
		return cached
	}
	if depth >= maximumChannelThreadStartDepth {
		return channelThreadStartResult{height: 1}
	}
	result := channelThreadStartResult{allowed: true, height: 1, complete: true}
	anchor := p.parents[id]
	if anchor == nil || !anchor.StartsThread || anchor.ThreadRootID != "" || viewer != nil && !CanViewChannelMessage(anchor, *viewer) {
		result.allowed = false
		return result
	}
	active[id] = true
	defer delete(active, id)
	if anchor.ReplyToMessageID != "" {
		parent := p.parents[anchor.ReplyToMessageID]
		if parent == nil || viewer != nil && !CanViewChannelMessage(parent, *viewer) {
			result.allowed = false
		} else {
			ancestors := []*ChannelMessage{parent}
			if !parent.StartsThread {
				for _, linked := range directChannelMessageParents(parent) {
					ancestors = append(ancestors, p.parents[linked])
				}
			}
			for _, ancestor := range ancestors {
				if ancestor == nil || viewer != nil && !CanViewChannelMessage(ancestor, *viewer) {
					result.allowed = false
					continue
				}
				if ancestor.StartsThread {
					child := p.visit(ancestor.ID, depth+1, viewer, active)
					result.allowed = result.allowed && child.allowed
					result.complete = result.complete && child.complete
					result.height = max(result.height, child.height+1)
				}
			}
		}
	}
	if result.complete {
		if p.results == nil {
			p.results = make(map[string]channelThreadStartResult)
		}
		p.results[id] = result
	}
	return result
}

func threadStartAnchors(message *ChannelMessage, known map[string]*ChannelMessage) []string {
	anchors := make([]string, 0, 3)
	if message == nil {
		return anchors
	}
	if message.StartsThread {
		anchors = append(anchors, message.ID)
	}
	for _, id := range directChannelMessageParents(message) {
		if parent := known[id]; parent != nil && parent.StartsThread {
			anchors = append(anchors, parent.ID)
		}
	}
	return anchors
}

func filterChannelMessageThreadStarts(ctx context.Context, scope Scope, conversationID string, messages []*ChannelMessage, known map[string]*ChannelMessage, load channelMessageParentLoader, viewer ConversationViewer) ([]*ChannelMessage, error) {
	proof := newChannelThreadStartProof(scope, conversationID, known, load)
	anchors := make([]string, 0)
	for _, message := range messages {
		if message != nil {
			anchors = append(anchors, threadStartAnchors(message, proof.parents)...)
		}
	}
	if len(anchors) == 0 {
		return messages, nil
	}
	if err := proof.preload(ctx, anchors, &viewer); err != nil {
		return nil, err
	}
	visible := make([]*ChannelMessage, 0, len(messages))
	for _, message := range messages {
		allowed := message != nil && message.Scope == scope && message.ConversationID == conversationID
		for _, id := range threadStartAnchors(message, proof.parents) {
			if !proof.visit(id, 0, &viewer, make(map[string]bool)).allowed {
				allowed = false
				break
			}
		}
		if allowed {
			visible = append(visible, message)
		}
	}
	return visible, nil
}

func (s *ConversationService) threadStartParentLoader(scope Scope, conversationID string) channelMessageParentLoader {
	if batch, ok := s.store.(ChannelMessageBatchStore); ok {
		return func(ctx context.Context, ids []string) (map[string]*ChannelMessage, error) {
			return loadChannelMessageBatches(ctx, batch, scope, conversationID, ids)
		}
	}
	return func(ctx context.Context, ids []string) (map[string]*ChannelMessage, error) {
		parents := make(map[string]*ChannelMessage, len(ids))
		for _, id := range ids {
			message, err := s.store.GetChannelMessage(ctx, scope, conversationID, id)
			if err != nil {
				return nil, err
			}
			parents[id] = message
		}
		return parents, nil
	}
}

func (s *ConversationService) validateThreadStartAncestry(ctx context.Context, message *ChannelMessage) (bool, error) {
	proof := newChannelThreadStartProof(message.Scope, message.ConversationID, map[string]*ChannelMessage{message.ID: message}, s.threadStartParentLoader(message.Scope, message.ConversationID))
	if err := proof.preload(ctx, []string{message.ID}, nil); err != nil {
		return false, err
	}
	return proof.visit(message.ID, 0, nil, make(map[string]bool)).allowed, nil
}
