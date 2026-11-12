package runtime

func (s *CollaborationService) SetAcceptedRunExecutionPreparer(preparer AcceptedRunExecutionPreparer) {
	if s != nil {
		s.executionPreparer = preparer
	}
}

func (r *AgentRequestInboxReconciler) SetAcceptedRunExecutionPreparer(preparer AcceptedRunExecutionPreparer) {
	if r != nil {
		r.collaboration.SetAcceptedRunExecutionPreparer(preparer)
	}
}
