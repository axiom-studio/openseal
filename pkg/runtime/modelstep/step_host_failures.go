package modelstep

// Codes a StepHost returns in TurnHostFailure.Code, so cortex, the daemon and
// the kernel agree on what each failure means.
//
// A provider that rejects the input for length, or a request too large to
// send, uses TurnHostContextExceeded: the kernel compacts and retries once.
const (
	// StepHostFailureUnreachable means nothing reached the host, so the
	// request is safe to send again, also for writes.
	StepHostFailureUnreachable = "step_host_unreachable"
	// StepHostFailureConnectionLost means the request was sent and no result
	// came back, so the outcome is unknown. Model steps are redone; host-tool
	// writes become interrupted.
	StepHostFailureConnectionLost = "step_host_connection_lost"
	// StepHostFailureDraining means the host is draining and refused before
	// doing any work.
	StepHostFailureDraining = "atlas_draining"
	// StepHostFailureProtocol means a frame, identity or envelope violated
	// the protocol. It is not retryable.
	StepHostFailureProtocol = "step_host_protocol"
	// StepHostFailureAccessDenied means the host answered 401 or 403.
	StepHostFailureAccessDenied = "step_host_access_denied"
)
