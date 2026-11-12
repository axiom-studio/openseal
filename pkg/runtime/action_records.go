package runtime

func containsActionStatus(values []ActionCallStatus, status ActionCallStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func containsApprovalStatus(values []ApprovalStatus, status ApprovalStatus) bool {
	for _, value := range values {
		if value == status {
			return true
		}
	}
	return false
}

func pageActionCalls(values []*ActionCall, offset, limit int) []*ActionCall {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*ActionCall{}
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

func pageApprovals(values []*ApprovalCheckpoint, offset, limit int) []*ApprovalCheckpoint {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*ApprovalCheckpoint{}
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
