# Product

## Register

product

## Users

Platform operators and engineers who manage automation workflows harmostes runs (documentation sync, PR review, fork maintenance). They are technical, comfortable in terminals, and use the UI as a faster alternative to kubectl. Their context: checking workflow health at a glance, drilling into a failed run, reading what the agent actually did (prompts, tool calls, gate feedback), and understanding why something failed. They value density over whitespace and information over polish.

## Product Purpose

Harmostes UI is the observability surface for the harmostes automation platform. It shows what the system is doing: which Attempts are running, what phase they are in, what the agent said and did, whether the gate passed, and where it broke. Configuration is code-first (GitOps YAML); the UI never creates or edits workflows. Its sole job is visibility: make the opaque (agent sessions, gate evaluations, orchestration history) readable.

## Brand Personality

**Retro-precise, dense, honest.**

Retro-precise: the RezusCloud design system is the visual language. Mac System 1 (1984) for light mode, NeXTSTEP (1988) for dark mode. Silkscreen for labels, VT323 for code, beveled borders in NeXT mode. The aesthetic is period-authentic computing, not retro-themed decoration.

Dense: old-school Grafana, not modern SaaS. Every pixel carries information. Data tables, monospace timestamps, status indicators packed tight. No hero metrics, no card grids, no empty whitespace for its own sake.

Honest: show what happened, not what looks good. Failed runs are red. Broken pipes are visible. Gate feedback is raw stderr. The UI does not dress up failures.

## Anti-references

- Jenkins: blue ocean dashboards, endless plugin UIs, corporate enterprise feel. The opposite of what this should look like.
- Generic SaaS observability: hero metric cards, gradient accents, "insights" panels that summarize what you could just see directly.
- Cloud provider consoles (AWS/GCP/Azure): infinite service catalogs, nested navigation, feature-matrix overload.
- Modern minimalist dashboards that sacrifice density for whitespace and round corners.

## Design Principles

1. **Density is respect.** The user is technical. Show them everything relevant in the minimum scroll. Data tables over card grids. Inline over modal. Expandable over paginated.

2. **The agent session is the product.** The transcript viewer (prompts, tool calls, gate feedback) is the primary reason the UI exists. It must be the most polished surface, not an afterthought.

3. **Retro is the language, not the decoration.** Silkscreen labels, VT323 monospace, beveled NeXT borders, Mac/NeXT dual theme. Every visual choice traces to the RezusCloud design system. No generic CSS variables that drift from the tokens.

4. **Read the machine, not the marketing.** Raw output over summaries. Gate stderr over "validation failed." Tool call results over "tool completed." The UI surfaces what the system produced, unedited.

5. **Code-first, UI-observe.** The UI never creates, edits, or deletes configuration — and has no write surfaces at all: no trigger, toggle, or creation flows. Workflows and templates are YAML in GitOps (Flux-provisioned); the UI reads and displays.

## Accessibility

WCAG AA following the RezusCloud design system. Keyboard navigation for all interactive elements. Full reduced-motion support. VT323 and Silkscreen are readable at the sizes used. No color-only status indicators (always paired with text or icon).
