package collaboration

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

func sameAgentRequestIntent(existing *models.AgentRequest, req CreateAgentRequestRequest) bool {
	acceptancePolicy := models.NormalizeAgentRequestAcceptancePolicy(req.AcceptancePolicy)
	if existing.DelegationPolicy != nil && existing.DelegationPolicy.RequireAcceptance {
		acceptancePolicy = models.AgentRequestAcceptanceRecipientReview
	}
	return existing.Kind == req.Kind && existing.Requester == req.Requester && existing.Recipient == req.Recipient &&
		existing.SourceRunID == strings.TrimSpace(req.SourceRunID) && existing.Goal == strings.TrimSpace(req.Goal) &&
		existing.Instructions == strings.TrimSpace(req.Instructions) && existing.SemanticRole == strings.TrimSpace(req.SemanticRole) &&
		existing.AcceptancePolicy == acceptancePolicy &&
		existing.DependencyGroupID == strings.TrimSpace(req.DependencyGroupID) && existing.DependencyID == strings.TrimSpace(req.DependencyID) &&
		sameJSONValue(existing.AcceptanceCriteria, req.AcceptanceCriteria) &&
		sameJSONValue(existing.ArtifactRequirements, req.ArtifactRequirements) &&
		sameJSONValue(existing.SharedContext, req.SharedContext) &&
		sameJSONValue(existing.ChildCheckpoint, req.ChildCheckpoint) &&
		sameJSONValue(existing.ConversationRefs, req.ConversationRefs) &&
		sameBudgetPolicy(existing.BudgetAllocation, req.BudgetAllocation)
}

func sameJSONValue(left, right interface{}) bool {
	if emptyJSONCollection(left) && emptyJSONCollection(right) {
		return true
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func emptyJSONCollection(value interface{}) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return (reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice) && reflected.Len() == 0
}

func sameBudgetPolicy(left, right *models.BudgetPolicy) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func StableCollaborationID(scope models.Scope, key, suffix string) string {
	seed := strings.Join([]string{"openseal", "collaboration", scope.Key(), strings.TrimSpace(key), strings.TrimSpace(suffix)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func sameAgentRequestCompletion(existing *models.AgentRequest, req CompleteAgentRequestRequest, artifacts []models.ArtifactReference) bool {
	left, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []models.ArtifactReference
	}{existing.CompletionSummary, existing.AcceptanceEvidence, existing.Artifacts})
	right, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []models.ArtifactReference
	}{strings.TrimSpace(req.Summary), req.AcceptanceEvidence, artifacts})
	return bytes.Equal(left, right)
}
