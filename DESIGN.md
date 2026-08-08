# Design

## Visual Theme

Dual-era retro computing (RezusCloud Design System). Mac System 1 (1984) light mode: warm amber-tinted neutrals, 1px rule borders, paper texture feel. NeXTSTEP (1988) dark mode: cool slate neutrals, beveled 2px borders, teal accent. The theme toggle shifts between two chapters of the same computing story.

## Color Palette (OKLCH)

### Mac mode (light)

| Token | Value | Role |
|-------|-------|------|
| paper | `oklch(99.5% 0.004 85)` | Page background |
| surface | `oklch(95.5% 0.004 85)` | Card/panel background |
| surface-strong | `oklch(88.5% 0.006 85)` | Sidebar, headers |
| ink | `oklch(14% 0.008 65)` | Primary text |
| ink-muted | `oklch(30% 0.008 65)` | Secondary text |
| rule | `oklch(72% 0.005 85)` | Borders, dividers |
| accent-gold | `oklch(78% 0.16 75)` | Accent, links |

### NeXT mode (dark)

| Token | Value | Role |
|-------|-------|------|
| next-black | `oklch(6% 0.005 270)` | Page background |
| next-dark | `oklch(20% 0.006 270)` | Sidebar, panels |
| next-mid | `oklch(34% 0.006 270)` | Surface variation |
| next-light | `oklch(58% 0.006 270)` | Borders, secondary text |
| next-white | `oklch(88% 0.004 270)` | Primary text |
| next-teal | `oklch(60% 0.08 170)` | Accent, active states |

### Semantic

| Token | Mac | NeXT |
|-------|-----|------|
| positive | `oklch(55% 0.12 150)` | `oklch(75% 0.14 150)` |
| negative | `oklch(50% 0.12 25)` | `oklch(65% 0.14 25)` |

## Typography

| Family | Font | Weight | Purpose |
|--------|------|--------|---------|
| display | Silkscreen | 700 | Headings, labels, nav, badges |
| body | system-ui | 400 | Paragraph copy, descriptions |
| mono | VT323 | 400 | Terminal output, code, timestamps, tool results |

### Scale

| Level | Size | Usage |
|-------|------|-------|
| Display | text-8xl | (not used in product UI) |
| Headline | text-2xl to text-3xl | Page titles |
| Title | text-lg to text-xl | Section headings |
| Body | text-sm to text-lg | Descriptions, content |
| Label | text-xs, uppercase, tracking-widest | Nav links, badges, status, meta |
| Mono | text-sm to text-base | Code, timestamps, tool I/O |

## Components

### Sidebar
- Width: 14rem
- Mac: surface-strong bg, 1px right border, active pill bg
- NeXT: next-dark bg, 2px beveled right border, beveled active state

### Data Table
- Mac: 1px rule borders, alternating paper/surface rows
- NeXT: minimal borders, alternating next-black/next-dark rows
- Cell padding: 0.75rem horizontal, 0.5rem vertical

### Status Indicators
- Status dot: small colored circle + text label (never color alone)
- Positive: green dot + "Green"/"Validated"/"Ready"
- Negative: red dot + "Failed"/"Error"
- Warning: amber dot + "Reconciling"/"Running"
- Neutral: gray dot + "Idle"/"Superseded"

### Buttons
- Primary: accent bg (gold/teal), ink/next-black text
- Secondary: surface bg, 1px/2px border
- Danger: negative bg
- XS variant: text-xs, 0.125rem 0.5rem padding

### Code/Terminal Blocks
- VT323 font, surface/next-black bg
- No border radius (retro sharp corners)
- Scrollable with max-height

## Layout

- App shell: fixed sidebar (14rem) + scrollable main content
- Page header: title + meta + actions, separated by 1px/2px border
- Content sections: separated by border, no cards
- Dense spacing: 0.5rem to 1rem between elements, not 2rem+

## Motion

- Easings: ease-out-quart/quint/expo
- No bounce, no elastic
- Full reduced-motion support
- Minimal animation (status changes, expand/collapse)

## Borders

- Mac mode: 1px solid rule color
- NeXT mode: 2px beveled (light top-left, dark bottom-right)
- No border-radius (retro sharp corners) except where the design system explicitly uses it
