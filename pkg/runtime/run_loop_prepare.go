package runtime

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
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

type catalogModelStepPreparerConfig struct {
	// AgentInstructions is the definition's persona and soul
	// (hostedAgentInstructions); the binding's Prompt.AgentInstructions follow.
	AgentInstructions []string
	// SkillPrompts are the claim's authorized Skill instructions.
	SkillPrompts []HostedSkillPrompt
	// HostTools are the chat Workspace's host tools (HostToolSpecs), nil when
	// the claim has no chat Workspace.
	HostTools       []modelstep.HostToolSpec
	ModelCredential *capability.CredentialReference
	// ImageInput is true when the bound model accepts images: it adds the
	// media section and makes the renderer resolve image bytes.
	ImageInput bool
	// RoutineScript is the routine_script section text when the Run executes
	// a pinned routine command (run_loop_routine_script.go), "" otherwise.
	RoutineScript string
	Capabilities  modelstep.CapabilitySetConfig
	// Render sources for image parts; ImageInput above is authoritative.
	Render modelstep.SessionRenderOptions
}

type catalogModelStepPreparer struct {
	config   catalogModelStepPreparerConfig
	controls Registry // loopControlRegistry; tests substitute their own
}

var _ agentrun.ModelStepPreparer = (*catalogModelStepPreparer)(nil)

func newCatalogModelStepPreparer(config catalogModelStepPreparerConfig) *catalogModelStepPreparer {
	config.Render.ImageInput = config.ImageInput
	return &catalogModelStepPreparer{config: config, controls: loopControlRegistry()}
}

// loopControlRegistry is every control tool a Run loop runs: the built-in
// tools and the kernel-only runbook tools.
func loopControlRegistry() Registry {
	return NewRegistry(append(BuiltinTools(), runbookControlTools()...)...)
}

// ModelCredential is a copy of the model credential the prepared steps send
// (nil: the host's default).
func (p *catalogModelStepPreparer) ModelCredential() *capability.CredentialReference {
	return modelstep.CloneHostedCredentialReference(p.config.ModelCredential)
}

// CapabilitySet is the visible capability set of the step: the binding's
// authorized catalog with the loads on the session path. The tool executor
// builds the same set to run load_capabilities and search_capabilities.
func (p *catalogModelStepPreparer) CapabilitySet(in agentrun.StepInput) *modelstep.CapabilitySet {
	catalog := modelstep.CapabilityCatalog{SkillPrompts: p.config.SkillPrompts, HostTools: p.config.HostTools}
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
	return modelstep.NewPathCapabilitySet(catalog, in.Path, p.config.Capabilities)
}

func (p *catalogModelStepPreparer) PrepareModelStep(ctx context.Context, in agentrun.StepInput) (ModelStepRequest, error) {
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
	if in.Mode == agentrun.StepModeExplainFailure {
		req.Purpose = ModelStepPurposeExplanation
		req.Tools = nil
		req.Stream.FinalAnswerTool = ""
	}
	entries, err := modelstep.ProjectSessionPath(ctx, in.Path, p.config.Render)
	if err != nil {
		return ModelStepRequest{}, err
	}
	req.Entries = entries
	if len(req.Tools) > 0 {
		req.Tools = modelstep.ApplySurfaces(req.Tools, in.Path, req.Entries)
	}
	return req, nil
}

// tools is the harness and core part of the SPEC §5.1 list: harness
// (control tools in their fixed order, capability search/load, pinned host
// tools in HostToolSpecs order), then core (governed actions of the pinned
// set, sorted by tool name). The loaded group comes from the session path
// (applySurfaces): each tool exactly as its load froze it.
func (p *catalogModelStepPreparer) tools(binding *agentrun.TurnRunnerBinding, set *modelstep.CapabilitySet) []ModelStepTool {
	var harness, core []ModelStepTool
	for _, spec := range p.controls.Specs(binding.Authority) {
		harness = append(harness, spec.ModelStepTool())
	}
	if !binding.Authority.OffersCapabilities() {
		return harness
	}
	if set.Offered() {
		harness = append(harness, modelstep.SearchCapabilitiesSpec(), modelstep.LoadCapabilitiesSpec())
	}
	names := conversation.LoopGovernedToolNames(binding.ModelActions)
	visibleActions, _, visibleHost, _ := set.Visible()
	visibleHostOps := map[string]bool{}
	for _, spec := range visibleHost {
		visibleHostOps[spec.Operation] = true
	}
	for _, spec := range p.config.HostTools {
		if visibleHostOps[spec.Operation] && set.Pinned(modelstep.CapabilityWorkspacePrefix+spec.Operation) {
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

func hostModelStepTool(spec modelstep.HostToolSpec, group ModelStepToolGroup) ModelStepTool {
	return ModelStepTool{
		Name: spec.ToolName, Description: spec.Description,
		InputSchema: maps.Clone(spec.InputSchema), Group: group,
	}
}

func governedModelStepTool(action capability.ModelAction, name string, group ModelStepToolGroup) ModelStepTool {
	return ModelStepTool{
		Name: name, Description: action.Description,
		InputSchema: modelstep.GovernedToolInputSchema(action), Group: group,
		Capability: action.Name,
	}
}

// system is SPEC §6: persona then the binding's instructions; the Skill
// prompts visible from the start; the kernel sections in their fixed order,
// the static loading instructions, then the binding's sections.
func (p *catalogModelStepPreparer) system(binding *agentrun.TurnRunnerBinding, set *modelstep.CapabilitySet, tools []ModelStepTool) ModelStepSystem {
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
		if binding.Authority.OffersCapabilities() && set.Pinned(modelstep.CapabilityPromptPrefix+prompt.Reference) {
			system.SkillPrompts = append(system.SkillPrompts, prompt)
		}
	}

	offered := map[string]bool{}
	for _, tool := range tools {
		offered[tool.Name] = true
	}
	texts := map[string]string{}
	if offered[string(ControlToolReportProgress)] {
		texts[modelstep.LoopSectionProgressRules] = modelstep.LoopProgressRulesText
		if binding.Authority.TaskWork {
			texts[modelstep.LoopSectionProgressRules] = modelstep.LoopProgressRulesTaskWorkText
		}
	}
	if offered[string(ControlToolStartTask)] {
		texts[modelstep.LoopSectionConversationTasks] = modelstep.LoopConversationTasksText
	}
	for _, spec := range p.config.HostTools {
		if offered[spec.ToolName] {
			texts[modelstep.LoopSectionWorkspace] = modelstep.LoopWorkspaceText
			break
		}
	}
	if p.config.ImageInput {
		texts[modelstep.LoopSectionMedia] = modelstep.LoopMediaText
	}
	if text := strings.TrimSpace(p.config.RoutineScript); text != "" {
		texts[modelstep.LoopSectionRoutineScript] = text
	}
	for _, id := range modelstep.LoopKernelSectionOrder {
		if text, ok := texts[id]; ok {
			system.Sections = append(system.Sections, ModelStepSystemSection{ID: id, Title: modelstep.LoopKernelSectionTitles[id], Text: text})
		}
	}
	if binding.Authority.OffersCapabilities() && set.Offered() {
		system.Sections = append(system.Sections, ModelStepSystemSection{ID: loopSectionCapabilities, Title: loopSectionCapabilitiesTitle, Text: modelstep.CapabilityLoadingText})
	}
	system.Sections = append(system.Sections, binding.Prompt.Sections...)
	return system
}

// boundSkill reports whether the claim has a bound Skill beyond the
// always-present built-in one.
func (p *catalogModelStepPreparer) boundSkill(binding *agentrun.TurnRunnerBinding) bool {
	if !binding.Authority.OffersCapabilities() {
		return false
	}
	for _, action := range binding.ModelActions {
		if action.SkillID != modelstep.PinnedCapabilitySkillID {
			return true
		}
	}
	for _, prompt := range p.config.SkillPrompts {
		if prompt.SkillID != modelstep.PinnedCapabilitySkillID {
			return true
		}
	}
	return false
}

func (p *catalogModelStepPreparer) stream(ctx context.Context, binding *agentrun.TurnRunnerBinding, tools []ModelStepTool) ModelStepStream {
	stream := ModelStepStream{
		AnswerPreview:        HasTurnAnswerPreview(ctx),
		ExternalChannelReply: binding.Authority.ExternalChannelReply,
		Spoken:               binding.Authority.EffectiveFinalMode() == conversationrun.FinalModeSpoken,
	}
	for _, tool := range tools {
		if tool.Name == string(ControlToolFinalAnswer) {
			stream.FinalAnswerTool = tool.Name
			break
		}
	}
	return stream
}

// loopCapabilityToolName is the toolName argument of ExecuteLoadCapabilities:
// the provider tool name of a newly visible action or host tool, "" for
// instructions and runbooks (run through run_operation).
func loopCapabilityToolName(actions []capability.ModelAction) func(id string) string {
	names := conversation.LoopGovernedToolNames(actions)
	return func(id string) string {
		switch {
		case strings.HasPrefix(id, capabilityActionPrefix):
			return names[strings.TrimPrefix(id, capabilityActionPrefix)]
		case strings.HasPrefix(id, modelstep.CapabilityWorkspacePrefix):
			return modelstep.HostToolName(strings.TrimPrefix(id, modelstep.CapabilityWorkspacePrefix))
		}
		return ""
	}
}

// ControlToolSpecs lists the control tools offered under a in the stable
// advertised order (SPEC §5.1).
func ControlToolSpecs(a agentrun.ControlAuthority) []ControlToolSpec {
	return loopControlRegistry().Specs(a)
}

// ControlModelStepTools is ControlToolSpecs as harness tools of a model step.
func ControlModelStepTools(a agentrun.ControlAuthority) []ModelStepTool {
	specs := ControlToolSpecs(a)
	tools := make([]ModelStepTool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, spec.ModelStepTool())
	}
	return tools
}

// CapabilityListing is the step's listing: the catalog (every ID, or the
// directory past the catalog budget), the Agents and
// operations the control tools accept, surfaced tools that are no longer in
// the catalog, the guidance that depends on the catalog, and the
// conversation's approval mode.
func (p *catalogModelStepPreparer) CapabilityListing(in agentrun.StepInput) modelstep.CapabilityListing {
	listing := modelstep.CapabilityListing{Mode: modelstep.CapabilityNoteModeNone}
	binding := in.Binding
	if binding == nil {
		return listing
	}
	authority := binding.Authority
	if authority.OffersCapabilities() {
		set := p.CapabilitySet(in)
		switch {
		case !set.Offered():
			// No catalog at all: nothing to list (mode none).
		case set.Searching():
			listing.Mode = modelstep.CapabilityNoteModeDirectory
			listing.Items = append(listing.Items, set.DirectoryLines()...)
		default:
			listing.Mode = modelstep.CapabilityNoteModeList
			listing.Items = append(listing.Items, set.CatalogLines()...)
		}
		for _, load := range modelstep.SurfacedOnPath(in.Path) {
			for _, tool := range load.Tools {
				if !set.Known(tool.Capability) {
					listing.Items = append(listing.Items, modelstep.CapabilityNoteItem{
						Key: "unavailable:" + tool.Name, Section: modelstep.CapabilityNoteUnavailable, Text: "- " + tool.Name,
					})
				}
			}
		}
	}
	listing.Items = append(listing.Items, modelstep.ControlTargetItems(authority)...)
	if authority.OffersCapabilities() && authority.ApprovalMode != "" {
		listing.Items = append(listing.Items, modelstep.CapabilityNoteItem{Key: modelstep.CapabilityNoteApprovalKey, Section: modelstep.CapabilityNoteGuidance, Text: conversation.ApprovalModeText(authority.ApprovalMode)})
	}
	if p.boundSkill(binding) {
		listing.Items = append(listing.Items, modelstep.CapabilityNoteItem{Key: "guide:skill_access", Section: modelstep.CapabilityNoteGuidance, Text: modelstep.LoopSkillAccessText})
	}
	for _, action := range binding.ModelActions {
		if authority.OffersCapabilities() && (modelstep.GovernedToolReviewed(action) || modelstep.GovernedToolNeedsReviewSummary(action)) {
			listing.Items = append(listing.Items, modelstep.CapabilityNoteItem{Key: "guide:review_summaries", Section: modelstep.CapabilityNoteGuidance, Text: modelstep.LoopReviewSummariesText})
			break
		}
	}
	return listing
}
