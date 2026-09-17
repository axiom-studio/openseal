package server

import (
	"net/http"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// Channel activity is selected before pagination, using the canonical channel
// owner and run context rather than a client-supplied team or a text search.
func (s *Server) handleListConversationRuns(w http.ResponseWriter, r *http.Request) {
	service, scope, id, ok := s.conversationRequestContext(w, r)
	if !ok {
		return
	}
	if s.desktopConversationScope != nil && scope != *s.desktopConversationScope {
		s.respondError(w, http.StatusForbidden, "channel activity belongs to the configured workspace")
		return
	}
	channel, err := service.GetConversation(r.Context(), scope, id)
	if err != nil {
		s.respondConversationError(w, err)
		return
	}
	limit, err := boundedIntQuery(r, "limit", 20, 1, 100)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	offset, err := boundedIntQuery(r, "offset", 0, 0, 1_000_000)
	if err != nil {
		s.respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	runs, err := runtime.NewPortfolioService(s.store).ListAgentRuns(r.Context(), runtime.AgentRunFilter{
		Scope: scope, Owner: &channel.Owner, Kind: runtime.RunKindConversation,
		ConversationID: channel.ID, Order: runtime.AgentRunOrderCreatedDesc,
		Limit: limit, Offset: offset,
	})
	if err != nil {
		s.respondAgentRunError(w, err)
		return
	}
	s.respondJSON(w, http.StatusOK, runs)
}
