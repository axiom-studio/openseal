package sqlite

import "github.com/axiom-studio/openseal/pkg/models"

func containsActionStatus(values []models.ActionCallStatus, status models.ActionCallStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func containsApprovalStatus(values []models.ApprovalStatus, status models.ApprovalStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func pageActionCalls(values []*models.ActionCall, offset, limit int) []*models.ActionCall {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*models.ActionCall{}
	}
	values = values[offset:]
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}

func pageApprovals(values []*models.ApprovalCheckpoint, offset, limit int) []*models.ApprovalCheckpoint {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*models.ApprovalCheckpoint{}
	}
	values = values[offset:]
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(values) > limit {
		values = values[:limit]
	}
	return values
}
