package runtime

import (
	"sync"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// MemoryStore holds canonical kernel state in memory.
type MemoryStore struct {
	mu                       sync.RWMutex
	objectives               map[string]*Objective
	initiatives              map[string]*Initiative
	sourceObservations       map[string]*SourceObservation
	sourceObservationKeys    map[string]string
	sourceMonitorCheckpoints map[string]*SourceMonitorCheckpoint
	eventSourceCheckpoints   map[string]*EventSourceCheckpoint
	eventSourceSubscriptions map[string]*EventSourceSubscription
	eventSourceHealth        map[string]*EventSourceHealth
	outreachThreads          map[string]*OutreachThread
	outreachIdempotency      map[string]string
	agentRuns                map[string]*AgentRun
	activity                 map[string][]*ActivityEvent
	turns                    map[string]map[string]*AgentTurn
	actions                  map[string]*ActionCall
	approvals                map[string]*ApprovalCheckpoint
	actionKeys               map[string]string
	externalOperationKeys    map[string]string
	requests                 map[string]*AgentRequest
	requestKeys              map[string]string
	dependencyGroups         map[string]*RunDependencyGroup
	dependencyGroupKeys      map[string]string
	dependencies             map[string]map[string]*RunDependency
	artifacts                map[string]map[int64]*Artifact
	conversations            map[string]*Conversation
	conversationKeys         map[string]string
	channelMessages          map[string][]*ChannelMessage
	channelMessageIDs        map[string]*ChannelMessage
	channelMessageKeys       map[string]string
	participationRounds      map[string]*ParticipationRoundResult
	participationKeys        map[string]string
	conversationCursors      map[string]*ConversationCursor
	conversationPresence     map[string]*ConversationPresence
	externalEndpoints        map[string]*ExternalConversationEndpoint
	externalGateways         map[string]*ExternalConversationGatewayRegistration
	externalInbox            map[string]*ExternalConversationInboxItem
	externalInboxKeys        map[string]string
	externalMappings         map[string]*ExternalConversationMapping
	externalParticipants     map[string]*ExternalParticipantMapping
	externalMessages         map[string]*ExternalMessageMapping
	externalDeliveries       map[string]*ExternalConversationDelivery
	externalDeliveryKeys     map[string]string
	skillDefinitions         map[string]*skill.Definition
	skillBindings            map[string]*skill.Binding
}

// NewMemoryStore creates an in-memory canonical kernel store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		objectives:               make(map[string]*Objective),
		initiatives:              make(map[string]*Initiative),
		sourceObservations:       make(map[string]*SourceObservation),
		sourceObservationKeys:    make(map[string]string),
		sourceMonitorCheckpoints: make(map[string]*SourceMonitorCheckpoint),
		eventSourceCheckpoints:   make(map[string]*EventSourceCheckpoint),
		eventSourceSubscriptions: make(map[string]*EventSourceSubscription),
		eventSourceHealth:        make(map[string]*EventSourceHealth),
		outreachThreads:          make(map[string]*OutreachThread),
		outreachIdempotency:      make(map[string]string),
		agentRuns:                make(map[string]*AgentRun),
		activity:                 make(map[string][]*ActivityEvent),
		turns:                    make(map[string]map[string]*AgentTurn),
		actions:                  make(map[string]*ActionCall),
		approvals:                make(map[string]*ApprovalCheckpoint),
		actionKeys:               make(map[string]string),
		externalOperationKeys:    make(map[string]string),
		requests:                 make(map[string]*AgentRequest),
		requestKeys:              make(map[string]string),
		dependencyGroups:         make(map[string]*RunDependencyGroup),
		dependencyGroupKeys:      make(map[string]string),
		dependencies:             make(map[string]map[string]*RunDependency),
		artifacts:                make(map[string]map[int64]*Artifact),
		conversations:            make(map[string]*Conversation),
		conversationKeys:         make(map[string]string),
		channelMessages:          make(map[string][]*ChannelMessage),
		channelMessageIDs:        make(map[string]*ChannelMessage),
		channelMessageKeys:       make(map[string]string),
		participationRounds:      make(map[string]*ParticipationRoundResult),
		participationKeys:        make(map[string]string),
		conversationCursors:      make(map[string]*ConversationCursor),
		conversationPresence:     make(map[string]*ConversationPresence),
		externalEndpoints:        make(map[string]*ExternalConversationEndpoint),
		externalGateways:         make(map[string]*ExternalConversationGatewayRegistration),
		externalInbox:            make(map[string]*ExternalConversationInboxItem),
		externalInboxKeys:        make(map[string]string),
		externalMappings:         make(map[string]*ExternalConversationMapping),
		externalParticipants:     make(map[string]*ExternalParticipantMapping),
		externalMessages:         make(map[string]*ExternalMessageMapping),
		externalDeliveries:       make(map[string]*ExternalConversationDelivery),
		externalDeliveryKeys:     make(map[string]string),
		skillDefinitions:         make(map[string]*skill.Definition),
		skillBindings:            make(map[string]*skill.Binding),
	}
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
