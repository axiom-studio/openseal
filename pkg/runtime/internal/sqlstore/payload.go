package sqlstore

import (
	"encoding/json"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func DecodeActionCall(payload string) (*models.ActionCall, error) {
	var call models.ActionCall
	if err := json.Unmarshal([]byte(payload), &call); err != nil {
		return nil, err
	}
	return &call, nil
}

func DecodeApproval(payload string) (*models.ApprovalCheckpoint, error) {
	var approval models.ApprovalCheckpoint
	if err := json.Unmarshal([]byte(payload), &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}

func AgentRegistryPayloads(deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) (string, string, error) {
	deploymentPayload, err := json.Marshal(deployment)
	if err != nil {
		return "", "", err
	}
	activationPayload, err := json.Marshal(activation)
	if err != nil {
		return "", "", err
	}
	return string(deploymentPayload), string(activationPayload), nil
}

func DecodeArtifact(payload string) (*models.Artifact, error) {
	var artifact models.Artifact
	if err := json.Unmarshal([]byte(payload), &artifact); err != nil {
		return nil, err
	}
	return &artifact, nil
}

func DecodeAgentRequest(payload string) (*models.AgentRequest, error) {
	var request models.AgentRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return nil, fmt.Errorf("decode agent request: %w", err)
	}
	return &request, nil
}

func DecodeConversation(payload string) (*models.Conversation, error) {
	var value models.Conversation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	models.ApplyConversationDefaults(&value)
	return &value, nil
}

func DecodeChannelMessage(payload string) (*models.ChannelMessage, error) {
	var value models.ChannelMessage
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode channel message: %w", err)
	}
	return &value, nil
}

func DecodeParticipationRound(payload string) (*models.ParticipationRound, error) {
	var value models.ParticipationRound
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode participation round: %w", err)
	}
	return &value, nil
}

func DecodeConversationCursor(payload string) (*models.ConversationCursor, error) {
	var value models.ConversationCursor
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation cursor: %w", err)
	}
	return &value, nil
}

func DecodeConversationPresence(payload string) (*models.ConversationPresence, error) {
	var value models.ConversationPresence
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode conversation presence: %w", err)
	}
	return &value, nil
}

func DecodeRunDependencyGroup(payload string) (*models.RunDependencyGroup, error) {
	var group models.RunDependencyGroup
	if err := json.Unmarshal([]byte(payload), &group); err != nil {
		return nil, fmt.Errorf("decode run dependency group: %w", err)
	}
	return &group, nil
}

func DecodeRunDependency(payload string) (*models.RunDependency, error) {
	var edge models.RunDependency
	if err := json.Unmarshal([]byte(payload), &edge); err != nil {
		return nil, fmt.Errorf("decode run dependency: %w", err)
	}
	return &edge, nil
}

func DecodeObjective(payload string) (*models.Objective, error) {
	var objective models.Objective
	if err := json.Unmarshal([]byte(payload), &objective); err != nil {
		return nil, fmt.Errorf("decode objective: %w", err)
	}
	return &objective, nil
}

func DecodeAgentRun(payload string) (*models.AgentRun, error) {
	var run models.AgentRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return nil, fmt.Errorf("decode agent run: %w", err)
	}
	if run.Revision == 0 {
		run.Revision = 1
	}
	if run.AvailableAt.IsZero() {
		run.AvailableAt = run.CreatedAt
	}
	if run.QueueEnteredAt.IsZero() {
		run.QueueEnteredAt = run.CreatedAt
	}
	run.Kind = models.NormalizeRunKind(run.Kind)
	return &run, nil
}

func DecodeRunbookActivation(payload string) (*models.RunbookActivation, error) {
	var value models.RunbookActivation
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return &value, nil
}

func TeamRegistryPayloads(deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) (string, string, error) {
	deploymentPayload, err := json.Marshal(deployment)
	if err != nil {
		return "", "", err
	}
	activationPayload, err := json.Marshal(activation)
	if err != nil {
		return "", "", err
	}
	return string(deploymentPayload), string(activationPayload), nil
}

func DecodeAgentTurnRecord(payload string) (*models.AgentTurn, error) {
	var turn models.AgentTurn
	if err := json.Unmarshal([]byte(payload), &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}
