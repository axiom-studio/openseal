# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

The interface runs in a native Tauri desktop window on macOS, Windows, and Linux. Desktop is the shipping target; small browser widths support development and accessibility.

## Stack

Existing Go kernel and Rust daemon host. Working assumption after the instruction to move ahead: React, TypeScript, and Vite for the graphical interface, Tauri for native integration.

## Users

Working assumption: individuals and small teams who delegate substantive work to autonomous agents and need to inspect progress, review decisions, and recover durable work.

## Product Purpose

OpenSeal turns prompts into governed agents and teams, objectives, durable runs, activity, and artifacts. The desktop app makes those capabilities understandable and usable without a terminal.

## Capabilities and Constraints

The kernel and capability-discovered API remain authoritative. Never simulate successful work or show unavailable mutations as available. Runtime authentication tokens and saved credentials stay outside the webview. The user explicitly requested UI provider configuration: a masked input may hold a newly entered API key transiently, then clears after saving. Native ownership must not terminate independent daemons. Workspace data survives app restarts.

The first workflow is provisionally create an agent, review its proposal, and run work. Teams, work inspection, approvals, channels, marketplace, settings, and recovery remain part of the complete desktop scope.

## Brand Commitments

The user asks for a world-class, carefully detailed desktop app in the design tradition of Apple and Google. Favor clear hierarchy, legible typography, familiar desktop interaction, precise spacing, restrained color, and complete behavior. Competitive superiority is an ambition, not an established factual claim.

## Evidence on Hand

README.md, docs/dev-docs/api.md, docs/dev-docs/tui.md, pkg/client, pkg/capability, and internal/server define the real product. desktop/crates/daemon-host implements native local API ownership. No customer proof or benchmarks have been supplied.

## Product Principles

- Help people move from intent to useful work with the next action visible.
- Make authority, review requirements, and unavailable capabilities understandable.
- Preserve context and work across interruptions.
- Treat keyboard access, recovery, and empty states as primary experiences.

## Open Decisions

The audience and first-workflow prioritization are inferred, not confirmed. Platform-specific distribution, signing credentials, and the competitor evaluation set remain open.
