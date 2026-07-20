package server

import (
	"net/http"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)
	s.mux.HandleFunc("GET /api/v1/skills/{id}", s.handleGetSkill)
	s.mux.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}", s.handleGetWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows", s.handleCreateWorkflow)
	s.mux.HandleFunc("POST /api/v1/workflows/validate", s.handleValidateWorkflow)
	s.mux.HandleFunc("GET /api/v1/workflows/{id}/hcl", s.handleGetWorkflowHCL)
	s.mux.HandleFunc("POST /api/v1/workflows/{id}/run", s.handleRunWorkflow)
	s.mux.HandleFunc("GET /api/v1/runs", s.handleListRuns)
	s.mux.HandleFunc("GET /api/v1/runs/{id}", s.handleGetRun)
	s.mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("GET /api/v1/activity", s.handleListActivity)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/compile", s.handleCompileWorkforce)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets", s.handleCreateWorkforceChangeSet)
	s.mux.HandleFunc("GET /api/v1/authoring/workforce/change-sets/{id}", s.handleGetWorkforceChangeSet)
	s.mux.HandleFunc("PATCH /api/v1/authoring/workforce/change-sets/{id}/placement", s.handleUpdateWorkforceChangeSetPlacement)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/retry", s.handleRetryWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/evaluations", s.handleEvaluateWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/approvals", s.handleApproveWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/apply", s.handleApplyWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/objectives", s.handleCreateObjective)
	s.mux.HandleFunc("GET /api/v1/objectives", s.handleListObjectives)
	s.mux.HandleFunc("GET /api/v1/objectives/{id}", s.handleGetObjective)
	s.mux.HandleFunc("PUT /api/v1/objectives/{id}", s.handleUpdateObjective)
	s.mux.HandleFunc("POST /api/v1/events", s.handleRouteEvent)
	s.mux.HandleFunc("POST /api/v1/initiatives", s.handleCreateInitiative)
	s.mux.HandleFunc("GET /api/v1/initiatives", s.handleListInitiatives)
	s.mux.HandleFunc("GET /api/v1/initiatives/{id}", s.handleGetInitiative)
	s.mux.HandleFunc("PATCH /api/v1/initiatives/{id}", s.handlePatchInitiative)
	s.mux.HandleFunc("GET /api/v1/initiatives/{id}/source-monitors/{monitorId}/observations", s.handleListSourceObservations)
	s.mux.HandleFunc("GET /api/v1/initiatives/{id}/source-monitors/{monitorId}/checkpoint", s.handleGetSourceMonitorCheckpoint)
	s.mux.HandleFunc("POST /api/v1/initiatives/{id}/outreach", s.handleCreateOutreachThread)
	s.mux.HandleFunc("GET /api/v1/initiatives/{id}/outreach", s.handleListOutreachThreads)
	s.mux.HandleFunc("GET /api/v1/initiatives/{id}/outreach/{threadId}", s.handleGetOutreachThread)
	s.mux.HandleFunc("POST /api/v1/initiatives/{id}/outreach/{threadId}/messages/{messageId}/deliveries", s.handleDeliverOutreachMessage)
	s.mux.HandleFunc("GET /api/v1/clawhub/catalog/{reference}", s.handleInspectClawHub)
	s.mux.HandleFunc("GET /api/v1/clawhub/catalog/{reference}/versions", s.handleListClawHubVersions)
	s.mux.HandleFunc("GET /api/v1/clawhub/catalog/{reference}/file", s.handleGetClawHubFile)
	s.mux.HandleFunc("POST /api/v1/clawhub/catalog/{reference}/verify", s.handleVerifyClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/catalog/{reference}/install", s.handleInstallClawHub)
	s.mux.HandleFunc("GET /api/v1/clawhub/installed", s.handleListInstalledClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/installed/update-all", s.handleUpdateAllClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/installed/{reference}/verify", s.handleVerifyInstalledClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/installed/{reference}/pin", s.handlePinClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/installed/{reference}/unpin", s.handleUnpinClawHub)
	s.mux.HandleFunc("POST /api/v1/clawhub/installed/{reference}/update", s.handleUpdateClawHub)
	s.mux.HandleFunc("DELETE /api/v1/clawhub/installed/{reference}", s.handleUninstallClawHub)
	s.mux.HandleFunc("POST /api/v1/agent-runs", s.handleCreateAgentRun)
	s.mux.HandleFunc("GET /api/v1/agent-runs", s.handleListAgentRuns)
	s.mux.HandleFunc("GET /api/v1/agent-runs/{id}", s.handleGetAgentRun)
	s.mux.HandleFunc("POST /api/v1/agent-runs/{id}/commands", s.handleCommandAgentRun)
	s.mux.HandleFunc("POST /api/v1/agent-requests", s.handleCreateAgentRequest)
	s.mux.HandleFunc("GET /api/v1/agent-requests", s.handleListAgentRequests)
	s.mux.HandleFunc("GET /api/v1/agent-requests/{id}", s.handleGetAgentRequest)
	s.mux.HandleFunc("POST /api/v1/agent-requests/{id}/responses", s.handleRespondAgentRequest)
	s.mux.HandleFunc("POST /api/v1/agent-requests/{id}/completions", s.handleCompleteAgentRequest)
	s.mux.HandleFunc("GET /api/v1/action-approvals", s.handleListActionApprovals)
	s.mux.HandleFunc("GET /api/v1/action-approvals/{id}", s.handleGetActionApproval)
	s.mux.HandleFunc("POST /api/v1/action-approvals/{id}/decisions", s.handleResolveActionApproval)
	s.mux.HandleFunc("GET /api/v1/agent-deployments", s.handleListAgentDeployments)
	s.mux.HandleFunc("GET /api/v1/agent-deployments/{id}", s.handleGetAgentDeployment)
	s.mux.HandleFunc("GET /api/v1/agent-deployments/{id}/compilations", s.handleListAgentDefinitionCompilations)
	s.mux.HandleFunc("GET /api/v1/agent-deployments/{deploymentId}/skill-actions", s.handleListSkillActions)
	s.mux.HandleFunc("POST /api/v1/artifacts", s.handleRegisterArtifact)
	s.mux.HandleFunc("GET /api/v1/artifacts", s.handleListArtifacts)
	s.mux.HandleFunc("GET /api/v1/artifacts/{id}", s.handleGetArtifact)
	s.mux.HandleFunc("POST /api/v1/artifact-content", s.handleUploadArtifactContent)
	s.mux.HandleFunc("GET /api/v1/artifacts/{id}/content", s.handleDownloadArtifactContent)
	s.mux.HandleFunc("POST /api/v1/artifacts/{id}/resolve", s.handleResolveArtifactContent)
	s.mux.HandleFunc("POST /api/v1/conversations", s.handleCreateConversation)
	s.mux.HandleFunc("GET /api/v1/conversations", s.handleListConversations)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}", s.handleGetConversation)
	s.mux.HandleFunc("POST /api/v1/conversations/{id}/messages", s.handlePostChannelMessage)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/messages", s.handleListChannelMessages)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/messages/{messageId}", s.handleGetChannelMessage)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/changes", s.handleListConversationChanges)
	s.mux.HandleFunc("POST /api/v1/conversations/{id}/participation-rounds", s.handleCoordinateParticipation)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/participation-rounds", s.handleListParticipationRounds)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/participation-rounds/{roundId}", s.handleGetParticipationRound)
	s.mux.HandleFunc("PUT /api/v1/conversations/{id}/cursor", s.handleAdvanceConversationCursor)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/cursor", s.handleGetConversationCursor)
	s.mux.HandleFunc("PUT /api/v1/conversations/{id}/presence", s.handleSetConversationPresence)
	s.mux.HandleFunc("DELETE /api/v1/conversations/{id}/presence", s.handleReleaseConversationPresence)
	s.mux.HandleFunc("GET /api/v1/conversations/{id}/presence", s.handleListConversationPresence)
	s.mux.HandleFunc("POST /api/v1/team-definitions", s.handleRegisterTeamDefinition)
	s.mux.HandleFunc("GET /api/v1/team-definitions/{id}", s.handleGetTeamDefinition)
	s.mux.HandleFunc("POST /api/v1/team-deployments", s.handleCreateTeamDeployment)
	s.mux.HandleFunc("GET /api/v1/team-deployments", s.handleListTeamDeployments)
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}", s.handleGetTeamDeployment)
	s.mux.HandleFunc("PUT /api/v1/team-deployments/{id}", s.handleUpdateTeamDeployment)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/activations", s.handleActivateTeamDefinition)
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}/activations", s.handleListTeamDefinitionActivations)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/amendments", s.handleProposeTeamDefinitionAmendment)
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}/amendments", s.handleListTeamDefinitionAmendments)
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}/amendments/{amendmentId}", s.handleGetTeamDefinitionAmendment)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/amendments/{amendmentId}/evaluations", s.handleEvaluateTeamDefinitionAmendment)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/amendments/{amendmentId}/decisions", s.handleResolveTeamDefinitionAmendment)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/amendments/{amendmentId}/activations", s.handleActivateTeamDefinitionAmendment)
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	runOperations := []string{kernelapi.OperationGet, kernelapi.OperationList, kernelapi.OperationPause, kernelapi.OperationResume, kernelapi.OperationCancel, kernelapi.OperationIntervene}
	if s.agentRunCreation != nil {
		runOperations = append(runOperations, kernelapi.OperationCreate)
	}
	capabilities := []kernelapi.Capability{kernelapi.ObjectivesCapability(), kernelapi.EventRoutingCapability(), kernelapi.AgentRunsCapability(runOperations...), kernelapi.ActivityCapability()}
	if _, ok := s.store.(runtime.CollaborationKernelStore); ok {
		capabilities = append(capabilities, kernelapi.AgentRequestsCapability())
	}
	capabilities = append(capabilities, kernelapi.ActionApprovalsCapability(kernelapi.ActionApprovalCapabilityFeatures{
		Resolution: s.actionApprovalAuth != nil,
	}))
	if _, ok := s.store.(runtime.InitiativeStore); ok {
		capabilities = append(capabilities, kernelapi.InitiativesCapability())
	}
	if _, ok := s.store.(runtime.SourceMonitorStore); ok {
		capabilities = append(capabilities, kernelapi.SourceMonitorsCapability())
	}
	if _, outreachOK := s.store.(runtime.OutreachStore); outreachOK {
		_, initiativesOK := s.store.(runtime.InitiativeStore)
		_, sourcesOK := s.store.(runtime.SourceMonitorStore)
		_, actionsOK := s.store.(runtime.OutreachActionReader)
		if initiativesOK && sourcesOK && actionsOK {
			operations := []string{kernelapi.OperationGet, kernelapi.OperationList}
			if _, skillsOK := s.store.(skill.CatalogStore); skillsOK {
				operations = append(operations, kernelapi.OperationCreate)
			}
			if s.outreachDelivery != nil {
				operations = append(operations, kernelapi.OperationDeliver)
			}
			capabilities = append(capabilities, kernelapi.OutreachCapability(operations...))
		}
	}
	if _, ok := s.store.(runtime.ArtifactStore); ok {
		contentOperations := make([]string, 0, 3)
		if s.artifactContent != nil {
			contentOperations = append(contentOperations, kernelapi.OperationUpload, kernelapi.OperationDownload)
		}
		if s.artifactResolver != nil {
			contentOperations = append(contentOperations, kernelapi.OperationResolve)
		}
		capabilities = append(capabilities, kernelapi.ArtifactCapability(contentOperations...))
	}
	if _, ok := s.store.(runtime.ConversationStore); ok {
		capabilities = append(capabilities, kernelapi.ChannelsCapability(kernelapi.ChannelCapabilityFeatures{Coordination: true, Changes: true}))
	}
	if _, agentsOK := s.store.(kernelagent.Store); agentsOK {
		capabilities = append(capabilities, kernelapi.AgentDefinitionsCapability())
		if _, teamsOK := s.store.(kernelteam.Store); teamsOK {
			capabilities = append(capabilities, kernelapi.TeamDefinitionsCapability(kernelapi.TeamDefinitionCapabilityFeatures{Amendments: true}))
		}
	}
	if _, skillsOK := s.store.(skill.CatalogStore); skillsOK {
		capabilities = append(capabilities, kernelapi.SkillActionsCapability())
	}
	if s.authoring != nil {
		workforceCapability := kernelapi.WorkforceAuthoringCapability(kernelapi.WorkforceAuthoringCapabilityFeatures{
			ChangeSets: s.authoringRuns != nil && s.authoringWorker != nil,
		})
		if s.authoringChanges != nil {
			s.composeWorkforceLifecycleCapability(r, &workforceCapability)
		}
		capabilities = append(capabilities, workforceCapability)
	}
	if s.clawHub != nil {
		lifecycle := s.clawHub.ClawHubLifecycleCapabilities()
		if !s.clawHubMutations {
			read := lifecycle.Operations[:0]
			for _, operation := range lifecycle.Operations {
				switch operation {
				case "inspect_catalog", "inspect_versions", "inspect_files", "inspect_security", "inspect_installed", "verify", "verify_installed":
					read = append(read, operation)
				}
			}
			lifecycle.Operations = read
		}
		capabilities = append(capabilities, kernelapi.ClawHubLifecycleCapability(lifecycle))
	}
	s.respondJSON(w, http.StatusOK, kernelapi.NewCapabilityDocument(capabilities...))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, 200, map[string]string{"status": "ok"})
}
