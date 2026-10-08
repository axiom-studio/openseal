package runtime

import (
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// MemoryStore holds canonical kernel state in memory.
type MemoryStore struct {
	*kernelteam.MemoryStore
	skillSetupRequests                  map[string]*SkillSetupRequest
	credentialRequests                  map[string]*CredentialRequest
	mu                                  sync.RWMutex
	objectives                          map[string]*Objective
	runbookActivations                  map[string]*RunbookActivation
	projects                            map[string]*Project
	projectSkillReferences              map[memoryProjectSkillReferenceKey]map[string]struct{}
	sourceObservations                  map[string]*SourceObservation
	sourceObservationKeys               map[string]string
	sourceMonitorCheckpoints            map[string]*SourceMonitorCheckpoint
	eventSourceCheckpoints              map[string]*EventSourceCheckpoint
	eventSourceSubscriptions            map[string]*EventSourceSubscription
	eventSourceHealth                   map[string]*EventSourceHealth
	outreachThreads                     map[string]*OutreachThread
	outreachIdempotency                 map[string]string
	agentRuns                           map[string]*AgentRun
	runTerminalReports                  map[runTerminalReportKey]*RunTerminalReport
	runTerminalReportLatest             map[runTerminalReportFamily]runTerminalReportKey
	runTerminalReportQueue              memoryTerminalReportQueue
	runEventWaits                       map[memoryRunEventWaitKey]*RunEventWait
	runEventWaitActive                  map[memoryRunEventRunKey]memoryRunEventWaitKey
	runEventWaitIndex                   map[memoryRunEventSelector]map[memoryRunEventWaitKey]struct{}
	runEventWaitSelectorOrder           map[memoryRunEventSelector]*memoryOrderedIndexNode[memoryRunEventWaitKey]
	runEventWaitDueOrder                map[Scope]*memoryOrderedIndexNode[memoryRunEventWaitDueKey]
	runEventWaitDueKeys                 map[memoryRunEventWaitKey]memoryRunEventWaitDueKey
	runEventWaitNotifications           map[memoryRunEventReceiptKey]runEventNotification
	runEventNotificationOrder           map[Scope]*memoryOrderedIndexNode[memoryRunEventNotificationDueKey]
	runEventWaitScopeWork               map[Scope]time.Time
	runEventWaitScopeOrder              *memoryOrderedIndexNode[memoryRunEventScopeDueKey]
	runEventReceipts                    map[memoryRunEventReceiptKey]*RunEventReceipt
	runEventReceiptIndex                map[memoryRunEventSelector]map[memoryRunEventReceiptKey]struct{}
	runEventReceiptOrder                *memoryRunEventOrderNode
	runEventReceiptScopeOrder           map[Scope]*memoryRunEventOrderNode
	runEventRetentionCursor             *memoryRunEventOrderKey
	runEventReceiptPruneCursors         map[Scope]*memoryRunEventOrderKey
	runEventConsumptions                map[memoryRunEventReceiptKey]map[memoryRunEventRunKey]struct{}
	activity                            map[string][]*ActivityEvent
	turns                               map[string]map[string]*AgentTurn
	actions                             map[string]*ActionCall
	skillRuntimeUsage                   map[memorySkillRuntimeUsageKey]int
	skillRuntimeMaintenance             map[string]*SkillRuntimeMaintenance
	skillRuntimeMaintenanceWaiters      map[string]map[string]struct{}
	skillRuntimeMaintenanceWaiterKeys   map[string]string
	skillRuntimeUsageCalls              map[string]memorySkillRuntimeUsageEntry
	skillRuntimeReceiptRuns             map[string]map[string]struct{}
	skillRuntimeRunDependencies         map[string]map[memorySkillRuntimeUsageKey]struct{}
	skillRuntimeUnqualifiedDependencies map[memorySkillRuntimeUsageKey]int
	approvals                           map[string]*ApprovalCheckpoint
	actionKeys                          map[string]string
	externalOperationKeys               map[string]string
	requests                            map[string]*AgentRequest
	requestKeys                         map[string]string
	dependencyGroups                    map[string]*RunDependencyGroup
	dependencyWaitingGroups             map[Scope]*memoryOrderedIndexNode[string]
	dependencyGroupKeys                 map[string]string
	dependencies                        map[string]map[string]*RunDependency
	artifacts                           map[string]map[int64]*Artifact
	conversations                       map[string]*Conversation
	conversationTasks                   map[string]*ConversationTask
	conversationTaskWorkRuns            map[string]string
	conversationTaskOrder               map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey]
	conversationTaskActiveOrder         map[memoryConversationTaskIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey]
	conversationActiveRunOrder          map[memoryConversationActiveRunIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey]
	conversationActiveRunMembership     map[string]memoryConversationActiveRunMembership
	conversationActiveRunRoots          map[string]map[string]struct{}
	conversationActiveRunRootKeys       map[string]string
	conversationForegroundRunOrder      map[memoryConversationActiveRunIndexKey]*memoryOrderedIndexNode[memoryConversationTaskPageKey]
	conversationForegroundRunMembership map[string]memoryConversationActiveRunMembership
	conversationTaskDueOrder            map[Scope]*memoryOrderedIndexNode[memoryConversationTaskPageKey]
	conversationTaskDueMembership       map[string]memoryConversationTaskPageKey
	conversationKeys                    map[string]string
	channelMessages                     map[string][]*ChannelMessage
	channelMessageIDs                   map[string]*ChannelMessage
	channelMessageKeys                  map[string]string
	participationRounds                 map[string]*ParticipationRoundResult
	participationKeys                   map[string]string
	conversationCursors                 map[string]*ConversationCursor
	conversationPresence                map[string]*ConversationPresence
	embedInstallations                  map[string]*EmbedInstallation
	embedRoutes                         map[string]string
	embedSessions                       map[string]*EmbedSession
	embedConversationSessions           map[string]string
	externalEndpoints                   map[string]*ExternalConversationEndpoint
	externalGateways                    map[string]*ExternalConversationGatewayRegistration
	callbackRegistrations               map[string]*CallbackRegistration
	callbackEvents                      map[string]*CallbackEventReceipt
	externalInbox                       map[string]*ExternalConversationInboxItem
	externalInboxKeys                   map[string]string
	externalMappings                    map[string]*ExternalConversationMapping
	externalParticipants                map[string]*ExternalParticipantMapping
	externalMessages                    map[string]*ExternalMessageMapping
	externalDeliveries                  map[string]*ExternalConversationDelivery
	externalDeliveryKeys                map[string]string
	skillDefinitions                    map[string]*skill.Definition
	skillBindings                       map[string]*skill.Binding
	skillBindingFloors                  map[string]int64
}

// NewMemoryStore creates an in-memory canonical kernel store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		MemoryStore:                 kernelteam.NewMemoryStore(),
		skillSetupRequests:          make(map[string]*SkillSetupRequest),
		credentialRequests:          make(map[string]*CredentialRequest),
		objectives:                  make(map[string]*Objective),
		runbookActivations:          make(map[string]*RunbookActivation),
		projects:                    make(map[string]*Project),
		sourceObservations:          make(map[string]*SourceObservation),
		sourceObservationKeys:       make(map[string]string),
		sourceMonitorCheckpoints:    make(map[string]*SourceMonitorCheckpoint),
		eventSourceCheckpoints:      make(map[string]*EventSourceCheckpoint),
		eventSourceSubscriptions:    make(map[string]*EventSourceSubscription),
		eventSourceHealth:           make(map[string]*EventSourceHealth),
		outreachThreads:             make(map[string]*OutreachThread),
		outreachIdempotency:         make(map[string]string),
		agentRuns:                   make(map[string]*AgentRun),
		runEventWaits:               make(map[memoryRunEventWaitKey]*RunEventWait),
		runEventWaitActive:          make(map[memoryRunEventRunKey]memoryRunEventWaitKey),
		runEventWaitIndex:           make(map[memoryRunEventSelector]map[memoryRunEventWaitKey]struct{}),
		runEventWaitSelectorOrder:   make(map[memoryRunEventSelector]*memoryOrderedIndexNode[memoryRunEventWaitKey]),
		runEventWaitDueOrder:        make(map[Scope]*memoryOrderedIndexNode[memoryRunEventWaitDueKey]),
		runEventWaitDueKeys:         make(map[memoryRunEventWaitKey]memoryRunEventWaitDueKey),
		runEventWaitNotifications:   make(map[memoryRunEventReceiptKey]runEventNotification),
		runEventNotificationOrder:   make(map[Scope]*memoryOrderedIndexNode[memoryRunEventNotificationDueKey]),
		runEventWaitScopeWork:       make(map[Scope]time.Time),
		runEventReceipts:            make(map[memoryRunEventReceiptKey]*RunEventReceipt),
		runEventReceiptIndex:        make(map[memoryRunEventSelector]map[memoryRunEventReceiptKey]struct{}),
		runEventReceiptScopeOrder:   make(map[Scope]*memoryRunEventOrderNode),
		runEventReceiptPruneCursors: make(map[Scope]*memoryRunEventOrderKey),
		runEventConsumptions:        make(map[memoryRunEventReceiptKey]map[memoryRunEventRunKey]struct{}),
		activity:                    make(map[string][]*ActivityEvent),
		turns:                       make(map[string]map[string]*AgentTurn),
		actions:                     make(map[string]*ActionCall),
		skillRuntimeUsage:           make(map[memorySkillRuntimeUsageKey]int),
		skillRuntimeMaintenance:     make(map[string]*SkillRuntimeMaintenance),
		skillRuntimeUsageCalls:      make(map[string]memorySkillRuntimeUsageEntry),
		skillRuntimeReceiptRuns:     make(map[string]map[string]struct{}),
		approvals:                   make(map[string]*ApprovalCheckpoint),
		actionKeys:                  make(map[string]string),
		externalOperationKeys:       make(map[string]string),
		requests:                    make(map[string]*AgentRequest),
		requestKeys:                 make(map[string]string),
		dependencyGroups:            make(map[string]*RunDependencyGroup),
		dependencyGroupKeys:         make(map[string]string),
		dependencies:                make(map[string]map[string]*RunDependency),
		artifacts:                   make(map[string]map[int64]*Artifact),
		conversations:               make(map[string]*Conversation),
		conversationKeys:            make(map[string]string),
		channelMessages:             make(map[string][]*ChannelMessage),
		channelMessageIDs:           make(map[string]*ChannelMessage),
		channelMessageKeys:          make(map[string]string),
		participationRounds:         make(map[string]*ParticipationRoundResult),
		participationKeys:           make(map[string]string),
		conversationCursors:         make(map[string]*ConversationCursor),
		conversationPresence:        make(map[string]*ConversationPresence),
		embedInstallations:          make(map[string]*EmbedInstallation),
		embedRoutes:                 make(map[string]string),
		embedSessions:               make(map[string]*EmbedSession),
		embedConversationSessions:   make(map[string]string),
		externalEndpoints:           make(map[string]*ExternalConversationEndpoint),
		externalGateways:            make(map[string]*ExternalConversationGatewayRegistration),
		callbackRegistrations:       make(map[string]*CallbackRegistration),
		callbackEvents:              make(map[string]*CallbackEventReceipt),
		externalInbox:               make(map[string]*ExternalConversationInboxItem),
		externalInboxKeys:           make(map[string]string),
		externalMappings:            make(map[string]*ExternalConversationMapping),
		externalParticipants:        make(map[string]*ExternalParticipantMapping),
		externalMessages:            make(map[string]*ExternalMessageMapping),
		externalDeliveries:          make(map[string]*ExternalConversationDelivery),
		externalDeliveryKeys:        make(map[string]string),
		skillDefinitions:            make(map[string]*skill.Definition),
		skillBindings:               make(map[string]*skill.Binding),
		skillBindingFloors:          make(map[string]int64),
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
