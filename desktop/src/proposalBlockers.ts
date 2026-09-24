import type { Proposal } from "./api";

type Result = NonNullable<Proposal["result"]>;
type MissingRequirement = NonNullable<Result["missingRequirements"]>[number];
type ValidationIssue = NonNullable<Result["validation"]>[number];

export function missingRequirementHelp(requirement: MissingRequirement) {
  const { kind, id } = requirement;
  switch (kind) {
    case "skill":
      return {
        blocker: `Required Skill ${id} is not available in this workspace.`,
        fix: `Use an operator-enabled OpenSeal Marketplace for this workspace to install ${id}, then create a fresh proposal. This desktop review cannot install Skills.`,
      };
    case "skill_installation":
      return {
        blocker: `Skill ${id} still needs installation.`,
        fix: `Install the exact version through the Marketplace, then create a fresh proposal. If this Skill has no verified ClawHub source in the proposal, use an operator-enabled client.`,
      };
    case "skill_unavailable":
      return {
        blocker: `Skill ${id} is unavailable.`,
        fix: `Use an operator-enabled OpenSeal Marketplace for this workspace to make ${id} available, then create a fresh proposal; or request a different Skill.`,
      };
    case "skill_binding":
      return {
        blocker: `Skill ${id} has no ready binding.`,
        fix: `Select the Skill settings and credential connection below, then save the configuration. You can add a new local connection if needed.`,
      };
    case "credential":
      return {
        blocker: `Required credential ${id} is not bound.`,
        fix: `Add a private local connection or select an existing one below. Then save the Skill configuration and review a fresh proposal if the catalog still lists this requirement.`,
      };
    case "prompt":
      return {
        blocker: `Skill ${id} cannot provide the requested prompt capability.`,
        fix: `Choose a Skill with prompt support or revise the proposal so this Skill is not used for prompts.`,
      };
    case "action":
      return {
        blocker: `Required Skill action ${id} is unavailable.`,
        fix: `Choose a Skill version that provides this action or revise the proposed workflow to use an available action.`,
      };
    case "version":
      return {
        blocker: `The required Skill version ${id} does not match the available version.`,
        fix: `Install or select that exact version, or revise the proposal to use the available version.`,
      };
    case "conversation_adapter":
      return {
        blocker: `Conversation adapter ${id} is unavailable.`,
        fix: `Select a configured provider adapter for this channel or remove the channel from the proposal.`,
      };
    case "source_policy":
      return {
        blocker: `Required source policy ${id} is unavailable.`,
        fix: `Create or select the source policy in this workspace, then update the proposed monitor.`,
      };
    case "source_scope":
      return {
        blocker: `The proposed source is outside policy ${id}.`,
        fix: `Narrow the source to the policy's allowed scope or choose an approved policy.`,
      };
    default:
      return {
        blocker: `Required ${kind.replaceAll("_", " ")} ${id} is unresolved.`,
        fix: `Review the requirement for ${requirement.requiredBy}, update the proposal or placement, then check again.`,
      };
  }
}

export function validationIssueHelp(issue: ValidationIssue) {
  const code = issue.code;
  if (code.includes("credential_unsatisfied"))
    return "Use an operator client to store the named credential and bind its reference to the exact Skill action; do not enter the secret here.";
  if (code.includes("approval_unreachable"))
    return "Configure a reviewed approval destination for this action, then check activation again.";
  if (code.includes("binding_missing") || code.includes("binding_ambiguous"))
    return "Use Workforce placement in the terminal client to select one exact installed Skill version and binding for this workflow action.";
  if (code.includes("binding_disabled") || code.includes("not_authorized"))
    return "Enable the exact Skill binding and allow this action in its binding policy.";
  if (code.includes("risk_exceeds_binding"))
    return "Choose a binding whose maximum risk permits this action, or remove the action.";
  if (code.includes("idempotency_required"))
    return "Use a Skill action that supports idempotency for durable side effects, or remove this workflow step.";
  if (code.includes("compensation_required"))
    return "Choose an action with a declared compensation action, or remove this destructive step.";
  if (code.includes("terminal_path_missing"))
    return "Connect this workflow entrypoint to a complete path ending in an end step.";
  if (code.includes("budget_"))
    return "Increase the workflow budget to cover this path or shorten the path.";
  if (code === "skill_binding_definition_unavailable")
    return "Use the terminal client's Marketplace and Workforce placement to select one installed, source-qualified Skill version before review.";
  if (code === "agent_deployment_identity_conflict")
    return "Choose a new Agent deployment identity or amend the existing Agent.";
  if (code === "no_speaking_role")
    return "Give at least one Team role active channel participation, then regenerate the proposal.";
  if (
    [
      "unknown_role",
      "role_mismatch",
      "role_bounds",
      "invalid_assignment",
    ].includes(code)
  )
    return "Revise the Team roles or assignments so every Agent meets its role's requirements, then regenerate the proposal.";
  return "Revise the indicated proposal field, then regenerate or check the proposal again.";
}
