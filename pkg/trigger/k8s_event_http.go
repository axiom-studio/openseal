package trigger

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/repository"
	envRepository "github.com/axiom-studio/openseal/pkg/environment"
	"go.uber.org/zap"
)

type K8sEventTriggerHTTP struct {
	mu              sync.RWMutex
	instances       map[int]*k8sEventInstanceHTTP
	pollers         map[int]context.CancelFunc
	handler         TriggerHandler
	started         bool
	k8sClient       *K8sEventClient
	instanceRepo    repository.AgentInstanceRepository
	environmentRepo envRepository.EnvironmentRepository
	logger          *zap.SugaredLogger
	seenEvents      map[int]map[string]bool
}

type k8sEventInstanceHTTP struct {
	instanceId         int
	clusterId          int
	namespace          string
	reason             string
	eventType          string
	involvedObjectKind string
	config             map[string]interface{}
}

func NewK8sEventTriggerHTTP(
	k8sClient *K8sEventClient,
	instanceRepo repository.AgentInstanceRepository,
	environmentRepo envRepository.EnvironmentRepository,
	logger *zap.SugaredLogger,
) *K8sEventTriggerHTTP {
	return &K8sEventTriggerHTTP{
		instances:       make(map[int]*k8sEventInstanceHTTP),
		pollers:         make(map[int]context.CancelFunc),
		k8sClient:       k8sClient,
		instanceRepo:    instanceRepo,
		environmentRepo: environmentRepo,
		logger:          logger,
		seenEvents:      make(map[int]map[string]bool),
	}
}

func (t *K8sEventTriggerHTTP) Type() string {
	return "k8s-event"
}

func (t *K8sEventTriggerHTTP) ValidateConfig(config map[string]interface{}) error {
	return nil
}

func (t *K8sEventTriggerHTTP) Setup(ctx context.Context, instanceId int, config map[string]interface{}) (*TriggerMetadata, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	agentInstance, err := t.instanceRepo.FindById(instanceId)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent instance: %w", err)
	}

	environment, err := t.environmentRepo.FindById(agentInstance.EnvironmentId)
	if err != nil {
		return nil, fmt.Errorf("failed to get environment: %w", err)
	}

	namespace, _ := config["namespace"].(string)
	reason, _ := config["reason"].(string)
	eventType, _ := config["type"].(string)
	involvedObjectKind, _ := config["involvedObjectKind"].(string)

	instance := &k8sEventInstanceHTTP{
		instanceId:         instanceId,
		clusterId:          environment.ClusterId,
		namespace:          namespace,
		reason:             reason,
		eventType:          eventType,
		involvedObjectKind: involvedObjectKind,
		config:             config,
	}

	t.instances[instanceId] = instance
	t.seenEvents[instanceId] = make(map[string]bool)

	if t.started {
		t.startPoller(ctx, instance)
	}

	t.logger.Infow("✅ K8s event trigger configured (HTTP)",
		"instanceId", instanceId,
		"clusterId", environment.ClusterId,
		"namespace", namespace,
		"reason", reason,
	)

	return &TriggerMetadata{}, nil
}

func (t *K8sEventTriggerHTTP) Teardown(ctx context.Context, instanceId int) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cancel, exists := t.pollers[instanceId]; exists {
		cancel()
		delete(t.pollers, instanceId)
	}

	delete(t.instances, instanceId)
	delete(t.seenEvents, instanceId)

	t.logger.Infow("K8s event trigger torn down (HTTP)", "instanceId", instanceId)
	return nil
}

func (t *K8sEventTriggerHTTP) Start(ctx context.Context, handler TriggerHandler) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.logger.Infow("Starting K8s Event Trigger (HTTP polling)", "instanceCount", len(t.instances))

	t.handler = handler
	t.started = true

	if len(t.instances) == 0 {
		t.logger.Warn("No K8s event trigger instances configured")
		return nil
	}

	for _, instance := range t.instances {
		t.startPoller(ctx, instance)
	}

	t.logger.Info("K8s Event Trigger fully started and polling for events")
	return nil
}

func (t *K8sEventTriggerHTTP) startPoller(ctx context.Context, instance *k8sEventInstanceHTTP) {
	pollCtx, cancel := context.WithCancel(ctx)
	t.pollers[instance.instanceId] = cancel

	go t.pollEvents(pollCtx, instance)
}

func (t *K8sEventTriggerHTTP) pollEvents(ctx context.Context, instance *k8sEventInstanceHTTP) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	t.logger.Infow("Starting event poller",
		"instanceId", instance.instanceId,
		"clusterId", instance.clusterId,
		"namespace", instance.namespace,
	)

	for {
		select {
		case <-ctx.Done():
			t.logger.Infow("Stopping event poller", "instanceId", instance.instanceId)
			return
		case <-ticker.C:
			t.checkEvents(ctx, instance)
		}
	}
}

func (t *K8sEventTriggerHTTP) checkEvents(ctx context.Context, instance *k8sEventInstanceHTTP) {
	events, err := t.k8sClient.ListEvents(ctx, instance.clusterId, instance.namespace)
	if err != nil {
		t.logger.Errorw("failed to list events", "instanceId", instance.instanceId, "clusterId", instance.clusterId, "namespace", instance.namespace, "error", err)
		return
	}

	for _, event := range events {
		if t.matchesFilter(event, instance) {
			eventUID, _ := event["metadata"].(map[string]interface{})["uid"].(string)
			if eventUID == "" {
				continue
			}

			t.mu.Lock()
			if t.seenEvents[instance.instanceId][eventUID] {
				t.mu.Unlock()
				continue
			}
			t.seenEvents[instance.instanceId][eventUID] = true
			t.mu.Unlock()

			t.fireEvent(ctx, instance, event)
		}
	}
}

func (t *K8sEventTriggerHTTP) matchesFilter(event map[string]interface{}, instance *k8sEventInstanceHTTP) bool {
	if instance.reason != "" {
		reason, _ := event["reason"].(string)
		if reason != instance.reason {
			return false
		}
	}

	if instance.eventType != "" {
		eventType, _ := event["type"].(string)
		if eventType != instance.eventType {
			return false
		}
	}

	if instance.involvedObjectKind != "" {
		involvedObject, ok := event["involvedObject"].(map[string]interface{})
		if !ok {
			return false
		}
		kind, _ := involvedObject["kind"].(string)
		if kind != instance.involvedObjectKind {
			return false
		}
	}

	return true
}

func (t *K8sEventTriggerHTTP) fireEvent(ctx context.Context, instance *k8sEventInstanceHTTP, event map[string]interface{}) {
	reason, _ := event["reason"].(string)
	eventType, _ := event["type"].(string)
	message, _ := event["message"].(string)

	metadata, _ := event["metadata"].(map[string]interface{})
	involvedObject, _ := event["involvedObject"].(map[string]interface{})

	payload := map[string]interface{}{
		"reason":         reason,
		"type":           eventType,
		"message":        message,
		"metadata":       metadata,
		"involvedObject": involvedObject,
		"namespace":      instance.namespace,
	}

	triggerEvent := TriggerEvent{
		Type:      "k8s-event",
		Source:    fmt.Sprintf("cluster-%d/%s/events", instance.clusterId, instance.namespace),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	}

	t.logger.Infow("K8s event MATCHED filter - firing trigger",
		"instanceId", instance.instanceId,
		"reason", reason,
		"type", eventType,
		"message", message,
	)

	if err := t.handler(ctx, instance.instanceId, triggerEvent); err != nil {
		t.logger.Errorw("K8s event trigger handler failed",
			"instanceId", instance.instanceId,
			"error", err,
		)
	} else {
		t.logger.Infow("K8s event trigger handler succeeded", "instanceId", instance.instanceId)
	}
}

func (t *K8sEventTriggerHTTP) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for instanceId, cancel := range t.pollers {
		cancel()
		t.logger.Infow("Stopped event poller", "instanceId", instanceId)
	}

	t.pollers = make(map[int]context.CancelFunc)
	t.started = false
	t.handler = nil

	return nil
}
