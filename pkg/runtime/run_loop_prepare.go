package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// catalogModelStepPreparer builds the model request of one step of a catalog
// Agent Run (SPEC §5.1, §6). Everything it sends is a function of the claim's
// binding, its configuration and the session path, so two steps over the same
// path send byte-identical System and Tools, and a load only appends tools.
//
// It fills the whole request except MaxOutputTokens, which is the step's
// output reservation and is set by the ModelStepProducer after it planned the
// step budget (0 here means no Run limit).
//
// Nothing the catalog decides is in System or in a harness or core tool: the
// listing, the Agents and operations the control tools accept and the
// catalog-dependent guidance are in the capabilities note
// (capability_note.go), and loaded tools are frozen on their load entries
// (capability_surface.go). So System and the harness and core tools stay
// byte-identical across the steps and Runs of a session while the claim's
// authority is unchanged.

// loopSectionCapabilities carries the static loading instructions of a
// deferred capability set. It follows the kernel sections of
// loopKernelSectionOrder and precedes the binding's sections.
const loopSectionCapabilities = "capabilities"

const loopSectionCapabilitiesTitle = "Capabilities"

// governedToolPrefix starts every governed tool name, so actions never collide
// with kernel or host tool names.
const governedToolPrefix = "action_"

type catalogModelStepPreparerConfig struct {
	// AgentInstructions is the definition's persona and soul
	// (hostedAgentInstructions); the binding's Prompt.AgentInstructions follow.
	AgentInstructions []string
	// SkillPrompts are the claim's authorized Skill instructions.
	SkillPrompts []HostedSkillPrompt
	// HostTools are the chat Workspace's host tools (HostToolSpecs), nil when
	// the claim has no chat Workspace.
	HostTools       []HostToolSpec
	ModelCredential *capability.CredentialReference
	// ImageInput is true when the bound model accepts images: it adds the
	// media section and makes the renderer resolve image bytes.
	ImageInput bool
	// RoutineScript is the routine_script section text when the Run executes
	// a pinned routine command (run_loop_routine_script.go), "" otherwise.
	RoutineScript string
	Capabilities  CapabilitySetConfig
	// Render sources for image parts; ImageInput above is authoritative.
	Render sessionRenderOptions
}

type catalogModelStepPreparer struct {
	config   catalogModelStepPreparerConfig
	controls controlRegistry // loopControlRegistry; tests substitute their own
}

var _ ModelStepPreparer = (*catalogModelStepPreparer)(nil)

func newCatalogModelStepPreparer(config catalogModelStepPreparerConfig) *catalogModelStepPreparer {
	config.Render.ImageInput = config.ImageInput
	return &catalogModelStepPreparer{config: config, controls: loopControlRegistry()}
}

// loopControlRegistry is every control tool a Run loop runs: the built-in
// tools and the kernel-only runbook tools.
func loopControlRegistry() controlRegistry {
	return newControlRegistry(append(builtinControlTools(), runbookControlTools()...)...)
}

// CapabilitySet is the visible capability set of the step: the binding's
// authorized catalog with the loads on the session path. The tool executor
// builds the same set to run load_capabilities and search_capabilities.
func (p *catalogModelStepPreparer) CapabilitySet(in StepInput) *CapabilitySet {
	catalog := CapabilityCatalog{SkillPrompts: p.config.SkillPrompts, HostTools: p.config.HostTools}
	if in.Run != nil {
		catalog.Scope = in.Run.Scope
		catalog.AgentID = in.Run.AssignedAgentID
	}
	if in.Binding != nil {
		catalog.Actions = in.Binding.ModelActions
		catalog.RunbookOperations = in.Binding.RunbookOperations
		if in.Binding.DeploymentID != "" {
			catalog.AgentID = in.Binding.DeploymentID
		}
	}
	return NewPathCapabilitySet(catalog, in.Path, p.config.Capabilities)
}

func (p *catalogModelStepPreparer) PrepareModelStep(ctx context.Context, in StepInput) (ModelStepRequest, error) {
	if in.Run == nil || in.Step == nil || in.Binding == nil {
		return ModelStepRequest{}, errors.New("prepare model step: run, step and binding are required")
	}
	binding := in.Binding
	set := p.CapabilitySet(in)
	tools := WithFinalOutputProperties(p.tools(binding, set), in.Run, binding.FinalOutput)

	req := ModelStepRequest{
		APIVersion:        ModelStepAPIVersion,
		Purpose:           ModelStepPurposeStep,
		Scope:             in.Run.Scope,
		RunID:             in.Run.ID,
		StepID:            in.Step.ID,
		Attempt:           in.Attempt,
		AgentID:           binding.DeploymentID,
		DefinitionID:      binding.DefinitionID,
		DefinitionVersion: binding.DefinitionVersion,
		ModelCredential:   p.config.ModelCredential,
		Model:             binding.Model,
		System:            p.system(binding, set, tools),
		Tools:             tools,
		Stream:            p.stream(ctx, binding, tools),
	}
	if req.AgentID == "" {
		req.AgentID = in.Run.AssignedAgentID
	}
	switch {
	case in.Session != nil:
		req.SessionID = in.Session.ID
	case in.Path != nil && in.Path.Session != nil:
		req.SessionID = in.Path.Session.ID
	default:
		req.SessionID = in.Step.SessionID
	}
	if in.Mode == StepModeExplainFailure {
		req.Purpose = ModelStepPurposeExplanation
		req.Tools = nil
		req.Stream.FinalAnswerTool = ""
	}
	entries, err := projectSessionPath(ctx, in.Path, p.config.Render)
	if err != nil {
		return ModelStepRequest{}, err
	}
	req.Entries = entries
	if len(req.Tools) > 0 {
		req.Tools = applySurfaces(req.Tools, in.Path, req.Entries)
	}
	return req, nil
}

// tools is the harness and core part of the SPEC §5.1 list: harness
// (control tools in their fixed order, capability search/load, pinned host
// tools in HostToolSpecs order), then core (governed actions of the pinned
// set, sorted by tool name). The loaded group comes from the session path
// (applySurfaces): each tool exactly as its load froze it.
func (p *catalogModelStepPreparer) tools(binding *TurnRunnerBinding, set *CapabilitySet) []ModelStepTool {
	var harness, core []ModelStepTool
	for _, spec := range p.controls.specs(binding.Authority) {
		harness = append(harness, spec.ModelStepTool())
	}
	if !binding.Authority.offersCapabilities() {
		return harness
	}
	if set.Offered() {
		harness = append(harness, SearchCapabilitiesSpec(), LoadCapabilitiesSpec())
	}
	names := LoopGovernedToolNames(binding.ModelActions)
	visibleActions, _, visibleHost, _ := set.Visible()
	visibleHostOps := map[string]bool{}
	for _, spec := range visibleHost {
		visibleHostOps[spec.Operation] = true
	}
	for _, spec := range p.config.HostTools {
		if visibleHostOps[spec.Operation] && set.Pinned(capabilityWorkspacePrefix+spec.Operation) {
			harness = append(harness, hostModelStepTool(spec, ModelStepToolHarness))
		}
	}
	for _, action := range visibleActions {
		if set.Pinned(capabilityActionPrefix + action.Name) {
			core = append(core, governedModelStepTool(action, names[action.Name], ModelStepToolCore))
		}
	}
	sort.SliceStable(core, func(i, j int) bool { return core[i].Name < core[j].Name })
	all := append(harness, core...)
	// Names are unique by construction; a clash (a host tool shadowing a
	// kernel tool) keeps the first, and the later tool answers is_error.
	seen := map[string]bool{}
	out := all[:0]
	for _, tool := range all {
		if tool.Name == "" || seen[tool.Name] {
			continue
		}
		seen[tool.Name] = true
		out = append(out, tool)
	}
	return out
}

func hostModelStepTool(spec HostToolSpec, group ModelStepToolGroup) ModelStepTool {
	return ModelStepTool{
		Name: spec.ToolName, Description: spec.Description,
		InputSchema: cloneMap(spec.InputSchema), Group: group,
	}
}

func governedModelStepTool(action capability.ModelAction, name string, group ModelStepToolGroup) ModelStepTool {
	return ModelStepTool{
		Name: name, Description: action.Description,
		InputSchema: GovernedToolInputSchema(action), Group: group,
		Capability: action.Name,
	}
}

// system is SPEC §6: persona then the binding's instructions; the Skill
// prompts visible from the start; the kernel sections in their fixed order,
// the static loading instructions, then the binding's sections.
func (p *catalogModelStepPreparer) system(binding *TurnRunnerBinding, set *CapabilitySet, tools []ModelStepTool) ModelStepSystem {
	var system ModelStepSystem
	for _, text := range append(append([]string(nil), p.config.AgentInstructions...), binding.Prompt.AgentInstructions...) {
		if text = strings.TrimSpace(text); text != "" {
			system.AgentInstructions = append(system.AgentInstructions, text)
		}
	}
	_, prompts, _, _ := set.Visible()
	for _, prompt := range prompts {
		// Loaded instructions arrive in the load result, so the system
		// prefix stays stable when the model loads.
		if binding.Authority.offersCapabilities() && set.Pinned(capabilityPromptPrefix+prompt.Reference) {
			system.SkillPrompts = append(system.SkillPrompts, prompt)
		}
	}

	offered := map[string]bool{}
	for _, tool := range tools {
		offered[tool.Name] = true
	}
	texts := map[string]string{}
	if offered[string(ControlToolReportProgress)] {
		texts[loopSectionProgressRules] = loopProgressRulesText
		if binding.Authority.TaskWork {
			texts[loopSectionProgressRules] = loopProgressRulesTaskWorkText
		}
	}
	if offered[string(ControlToolStartTask)] {
		texts[loopSectionConversationTasks] = loopConversationTasksText
	}
	for _, spec := range p.config.HostTools {
		if offered[spec.ToolName] {
			texts[loopSectionWorkspace] = loopWorkspaceText
			break
		}
	}
	if p.config.ImageInput {
		texts[loopSectionMedia] = loopMediaText
	}
	if text := strings.TrimSpace(p.config.RoutineScript); text != "" {
		texts[loopSectionRoutineScript] = text
	}
	for _, id := range loopKernelSectionOrder {
		if text, ok := texts[id]; ok {
			system.Sections = append(system.Sections, ModelStepSystemSection{ID: id, Title: loopKernelSectionTitles[id], Text: text})
		}
	}
	if binding.Authority.offersCapabilities() && set.Offered() {
		system.Sections = append(system.Sections, ModelStepSystemSection{ID: loopSectionCapabilities, Title: loopSectionCapabilitiesTitle, Text: capabilityLoadingText})
	}
	system.Sections = append(system.Sections, binding.Prompt.Sections...)
	return system
}

// offersCapabilities is false for decision and review Runs: they are offered
// only their own final tool, no capability, host or governed tool.
func (a ControlAuthority) offersCapabilities() bool {
	mode := a.finalMode()
	return mode != FinalModeDecision && mode != FinalModeReview
}

// boundSkill reports whether the claim has a bound Skill beyond the
// always-present built-in one.
func (p *catalogModelStepPreparer) boundSkill(binding *TurnRunnerBinding) bool {
	if !binding.Authority.offersCapabilities() {
		return false
	}
	for _, action := range binding.ModelActions {
		if action.SkillID != pinnedCapabilitySkillID {
			return true
		}
	}
	for _, prompt := range p.config.SkillPrompts {
		if prompt.SkillID != pinnedCapabilitySkillID {
			return true
		}
	}
	return false
}

func (p *catalogModelStepPreparer) stream(ctx context.Context, binding *TurnRunnerBinding, tools []ModelStepTool) ModelStepStream {
	stream := ModelStepStream{
		AnswerPreview:        HasTurnAnswerPreview(ctx),
		ExternalChannelReply: binding.Authority.ExternalChannelReply,
		Spoken:               binding.Authority.finalMode() == FinalModeSpoken,
	}
	for _, tool := range tools {
		if tool.Name == string(ControlToolFinalAnswer) {
			stream.FinalAnswerTool = tool.Name
			break
		}
	}
	return stream
}

// LoopGovernedToolNames maps each action name to its provider tool name:
// "action_" plus the name with "." as "_". A name that does not fit the
// provider pattern, or that would repeat an earlier action's tool name, gets
// a bounded form with a hash of the action name. It depends only on the
// actions and their order, so the tool executor resolves the same names.
func LoopGovernedToolNames(actions []capability.ModelAction) map[string]string {
	names := make(map[string]string, len(actions))
	taken := map[string]bool{}
	for _, action := range actions {
		if _, done := names[action.Name]; done || action.Name == "" {
			continue
		}
		name := governedToolPrefix + strings.ReplaceAll(action.Name, ".", "_")
		if !modelStepToolNamePattern.MatchString(name) || taken[name] {
			name = hashedGovernedToolName(action.Name)
		}
		names[action.Name] = name
		taken[name] = true
	}
	return names
}

func hashedGovernedToolName(actionName string) string {
	sum := sha256.Sum256([]byte(actionName))
	var b strings.Builder
	for _, r := range actionName {
		if b.Len() >= 48 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return governedToolPrefix + b.String() + "_" + hex.EncodeToString(sum[:4])
}

// LoopGovernedAction resolves a provider tool name back to its action.
func LoopGovernedAction(actions []capability.ModelAction, toolName string) (capability.ModelAction, bool) {
	names := LoopGovernedToolNames(actions)
	for _, action := range actions {
		if name, ok := names[action.Name]; ok && name == toolName {
			return action, true
		}
	}
	return capability.ModelAction{}, false
}

// loopCapabilityToolName is the toolName argument of ExecuteLoadCapabilities:
// the provider tool name of a newly visible action or host tool, "" for
// instructions and runbooks (run through run_operation).
func loopCapabilityToolName(actions []capability.ModelAction) func(id string) string {
	names := LoopGovernedToolNames(actions)
	return func(id string) string {
		switch {
		case strings.HasPrefix(id, capabilityActionPrefix):
			return names[strings.TrimPrefix(id, capabilityActionPrefix)]
		case strings.HasPrefix(id, capabilityWorkspacePrefix):
			return hostToolName(strings.TrimPrefix(id, capabilityWorkspacePrefix))
		}
		return ""
	}
}
