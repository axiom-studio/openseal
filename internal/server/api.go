package server

import (
	"io/fs"
	"net/http"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/webui"
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
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/compile", s.handleCompileWorkforce)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets", s.handleCreateWorkforceChangeSet)
	s.mux.HandleFunc("GET /api/v1/authoring/workforce/change-sets/{id}", s.handleGetWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/retry", s.handleRetryWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/evaluations", s.handleEvaluateWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/approvals", s.handleApproveWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/authoring/workforce/change-sets/{id}/apply", s.handleApplyWorkforceChangeSet)
	s.mux.HandleFunc("POST /api/v1/objectives", s.handleCreateObjective)
	s.mux.HandleFunc("GET /api/v1/objectives", s.handleListObjectives)
	s.mux.HandleFunc("GET /api/v1/objectives/{id}", s.handleGetObjective)
	s.mux.HandleFunc("PUT /api/v1/objectives/{id}", s.handleUpdateObjective)
	s.mux.HandleFunc("POST /api/v1/agent-runs", s.handleCreateAgentRun)
	s.mux.HandleFunc("GET /api/v1/agent-runs", s.handleListAgentRuns)
	s.mux.HandleFunc("GET /api/v1/agent-runs/{id}", s.handleGetAgentRun)
	s.mux.HandleFunc("POST /api/v1/agent-runs/{id}/commands", s.handleCommandAgentRun)
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
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}", s.handleGetTeamDeployment)
	s.mux.HandleFunc("PUT /api/v1/team-deployments/{id}", s.handleUpdateTeamDeployment)
	s.mux.HandleFunc("POST /api/v1/team-deployments/{id}/activations", s.handleActivateTeamDefinition)
	s.mux.HandleFunc("GET /api/v1/team-deployments/{id}/activations", s.handleListTeamDefinitionActivations)

	// Serve static frontend files
	dist, err := fs.Sub(webui.Dist, "dist")
	if err == nil {
		s.mux.Handle("/", http.FileServer(http.FS(dist)))
	}
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	capabilities := []kernelapi.Capability{kernelapi.ObjectivesCapability(), kernelapi.AgentRunsCapability()}
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
		capabilities = append(capabilities, kernelapi.TeamChannelsCapability())
	}
	if _, agentsOK := s.store.(kernelagent.Store); agentsOK {
		if _, teamsOK := s.store.(kernelteam.Store); teamsOK {
			capabilities = append(capabilities, kernelapi.TeamDefinitionsCapability())
		}
	}
	if s.authoring != nil {
		workforceCapability := kernelapi.WorkforceAuthoringCapability(s.authoringRuns != nil && s.authoringWorker != nil)
		if s.authoringChanges != nil {
			s.composeWorkforceLifecycleCapability(r, &workforceCapability)
		}
		capabilities = append(capabilities, workforceCapability)
	}
	s.respondJSON(w, http.StatusOK, kernelapi.NewCapabilityDocument(capabilities...))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respondJSON(w, 200, map[string]string{"status": "ok"})
}
