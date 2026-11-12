package models

import "errors"

var ErrExternalOperationClaimed = errors.New("external operation is already claimed")

func ExternalOperationProtects(status ActionCallStatus) bool {
	switch status {
	case ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusRunning,
		ActionCallStatusSucceeded, ActionCallStatusCompensating, ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

// ExternalOperationConflictError identifies the prior authoritative receipt.
// The attempted proposal is not persisted.
type ExternalOperationConflictError struct {
	Prior *ActionCall
}

func (e *ExternalOperationConflictError) Error() string { return ErrExternalOperationClaimed.Error() }

func (e *ExternalOperationConflictError) Unwrap() error { return ErrExternalOperationClaimed }

func ExternalOperationClaimDigest(call *ActionCall) string {
	if call == nil || call.DuplicateOfActionCallID != "" || !ExternalOperationProtects(call.Status) {
		return ""
	}
	return call.ExternalOperationDigest
}

func ExternalOperationReceiptComplete(status ActionCallStatus) bool {
	return status == ActionCallStatusSucceeded || status == ActionCallStatusCompensated
}
