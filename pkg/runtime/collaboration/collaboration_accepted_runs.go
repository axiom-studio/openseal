package collaboration

import "github.com/axiom-studio/openseal/pkg/models"

func (s *Service) SetAcceptedRunExecutionPreparer(preparer models.AcceptedRunExecutionPreparer) {
	if s != nil {
		s.executionPreparer = preparer
	}
}

func (r *AgentRequestInboxReconciler) SetAcceptedRunExecutionPreparer(preparer models.AcceptedRunExecutionPreparer) {
	if r != nil {
		r.collaboration.SetAcceptedRunExecutionPreparer(preparer)
	}
}
