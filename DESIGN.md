# Design

## Visual Theme

**Operations console** — the visual language of k8s control-plane tooling: `kubectl -o wide` density with ergonomic finishing. Neutral graphite surfaces, monospace identifiers, status color as the only expressive device. The direction is deliberately copied from the orchestration canon (Argo Workflows, Temporal, Conductor, GitHub Actions) at the operator's direction: one row per execution, status chip first, nothing decorative.

Dark is the default theme (operators on consoles); a tuned light theme ships alongside via the toggle (`html.dark`).

## Color Palette (OKLCH)

Neutral ground, one blue accent, four semantic status hues. Everything else is grayscale.

- **Ground (dark):** bg `oklch(14.5% 0.006 250)`, elevated `17.5%`, tertiary `22%`
- **Ground (light):** bg `oklch(97% 0.002 250)`, elevated white
- **Foreground:** fg `oklch(92% 0.004 250)` / muted `64%` (dark); ink `21%` / muted `47%` (light)
- **Border:** `27%` (dark), `90%` (light) — hairlines, 1px
- **Accent (interactive):** blue `oklch(0.68 0.14 250)` dark / `0.55 0.16 253` light — links, active nav, running state
- **Status:** success `oklch(0.72 0.15 152)`, warning `oklch(0.78 0.14 80)`, danger `oklch(0.68 0.18 25)`, running = accent, neutral gray
- **Chips** render status at low opacity over the ground (`--chip-*-bg` ~12–14%) — color-coded, never color-only (chips pair color + label)

## Typography

System stacks only. Display faces are retired.

- **UI:** `system-ui, -apple-system, "Segoe UI", Roboto, …`
- **Identifiers, timestamps, counts:** `ui-monospace, "SF Mono", "Cascadia Mono", …` — every CR name, hash, and time is mono; attempt names render short (`workflow · hash8`), full names live one click away
- **Scale:** 0.72 / 0.8 / 0.85 / 0.9 / 1.05 / 1.25 rem; table body 0.8rem; page titles 1.25rem at weight 650

## Components

- **`.tbl`** — the primary object. Dense table: sticky uppercase micro-headers, 32px rows, hairline row borders, hover highlight, right-aligned tabular-numeric time/count columns, `.is-failed` row tint. Sub-rows (`.subrow`) render hidden and expand per-group via the chevron cell.
- **`.chip`** — status vocabulary shared by every view: `failed`, `in flight`, `reconciling`, `queued`, `armed`, `dispatch lost`, `verdict`, `validated`, `superseded`. One chip component, dot + label, soft background.
- **`.tabs` / `.seg`** — filter tabs (state) and window segmented control (24h · 7d · all), always with counts.
- **`.strip`** — the window at a glance: total / failed / in flight / verdicts.
- **`.alertline`** — aggregated recurring failures (e.g. multiple dispatch losses) as one actionable banner with an SVG marker. Repetition is collapsed, never repeated as rows.
- **Sidebar** — slim 190px, mono logo, icon+label links, active state = inset accent bar + tint, count badges where useful.
- **Empty states** — dashed-border card, one sentence, one next action.

## Layout

`app-layout` flex: 190px sidebar + fluid main (max 1400px, 1rem/1.5rem padding). Page header: title left, identity + theme toggle right. Below 900px the sidebar becomes a top bar and tables scroll horizontally inside their wrap.

## Principles

1. **The table is the product.** Every list is a real table with column headers — no text walls, ever.
2. **Failed first.** Failures rank above in-flight work, which ranks above outcomes and history, on every list.
3. **Humans read labels, machines read hashes.** Subjects render as PR numbers/targets; hashes and full CR names are drill-down.
4. **Repetition is a defect.** Identical rows collapse into counts; recurring failures collapse into one alert.
5. **Status color is the only decoration.** One blue accent for interactivity; every other hue is semantic.
6. **k8s-native density.** Console rows, tabular numerals, relative time. Density is respect — but it is *organized* density.
