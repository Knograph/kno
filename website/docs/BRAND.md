# Kno brand system

## 1. Brand concept — Measured Intelligence

Kno is not an abstract AI assistant. Kno is an instrument.

It observes. It compares. It measures. It isolates variables. It produces
evidence. It tells developers what data earned its place.

The site should feel like a **precision instrument for AI engineers**, not
an AI marketing website. Desired reading, in order:

- 5 seconds: "This is a serious developer tool."
- 15 seconds: "Oh, it measures the value of the data going into my agent."
- 30 seconds: "I want to run this against my own data."
- After browsing: "These people care about rigor."
- After the community section: "I can see exactly how I could contribute."

Core visual tension, in two words: **mass + measurement**.

- _Mass_: large blocks, substantial typography, bold silhouettes, high
  contrast, strong framing, oversized graphic moments.
- _Measurement_: thin rules, labels, coordinates, values, confidence
  intervals, tables, terminal output, annotations, grids, small technical
  type.

The visual metaphor throughout: `INPUT → MEASURE → DECISION`.

## 2. Logo usage

The supplied Kno symbol and wordmark are canonical. Do not redesign them.

- Variants shipped: symbol black/white, lockup black/white
  (`public/images/brand/`).
- Navigation: horizontal lockup.
- Footer: lockup, white on black.
- Favicon: standalone symbol (black ground, white mark).
- Standalone symbol elsewhere: hero (oversized, cropped), final CTA.
  One dramatic oversized symbol beats twenty small ones.

Never: stretch, skew, outline, add gradients or shadows, recolor
arbitrarily, redraw, animate internal paths, or place in unnecessary
badges. Keep generous clearspace.

## 3. Color system

Fundamentally monochrome. The overall impression from ten feet away must
read as black and white.

| Token                 | Value     | Use                                   |
| --------------------- | --------- | ------------------------------------- |
| `--black`             | `#0a0a0a` | Dark sections, primary buttons, rules |
| `--graphite`          | `#1a1a1a` | Hover states, dark panels             |
| `--paper`             | `#f4f3ef` | Editorial ground                      |
| `--white`             | `#ffffff` | Content surfaces                      |
| `--muted` / `--faint` | grays     | Secondary text, labels                |

Semantic accents exist only to communicate **information**:

| Token    | Value     | Meaning                                 |
| -------- | --------- | --------------------------------------- |
| `--pos`  | `#0e7a3d` | Positive measured impact, KEEP, shipped |
| `--neg`  | `#c0352b` | Negative measured impact, REJECT        |
| `--warn` | `#8a6d00` | Uncertainty, PLANNED                    |
| `--info` | `#2456a6` | Selection, links, active states         |

Rules: semantic color is never the only channel — every use is paired with
a sign (`+18.4%`, `-8.9%`) or a text label (`KEEP`, `REJECT`, `PLANNED`).
Semantic colors never decorate; they annotate.

Section rhythm: hero black → problem white → outcomes paper → evidence
white → mechanism black → use cases white → comparison paper → quickstart
white → objections paper → community white → status paper (black panel) →
final CTA black. Contrast serves the story; do not alternate mechanically.

## 4. Typography

Three voices, all self-hosted variable fonts (no CDN, no third-party
requests):

1. **Display — Space Grotesk.** Substantial, compact, architectural.
   Headlines, buttons, section titles. Never thin weights.
2. **Editorial — Inter.** Serious technical-publishing body. Line length
   controlled to ~70ch (`--container-narrow`).
3. **Mono — JetBrains Mono.** CLI output, measurements, data values,
   annotations, metadata, labels, commands, diagrams. Mono is a brand
   voice, not just a code style: tiny interface labels like
   `[07 / QUICKSTART]`, `ASSET`, `RUN 0142` are set in mono uppercase with
   wide letter-spacing.

Hierarchy uses dramatic contrast: display headlines against 2xs mono
labels. Never make every piece of text the same medium sans.

## 5. Grid, spacing, geometry

- Container: 76rem full, 46rem narrow. 4px base spacing scale.
- Asymmetric compositions preferred; large margins; strong vertical
  rhythm; occasional full-bleed black sections.
- Section heads: `[01 / LABEL]` mono index, display heading, optional
  lede. Section numbers persist at every breakpoint.
- Geometry vocabulary (limited): diagonal notches on primary buttons,
  oversized cropped symbol, square corners everywhere. `--radius: 0`.
  Normal controls stay familiar — do not turn every element into a
  polygon.

Forbidden layout: `heading / paragraph / three rounded cards` as the
default pattern. Use numbered editorial rows, ruled tables, and terminal
panels instead. Cards exist only where structure justifies them.

## 6. Depth

Depth comes from scale, typography, layering, contrast, and whitespace —
never from shadows, glass, or elevation. Surfaces are flat; boundaries are
1px rules.

## 7. Terminal styling

Terminals are scientific output panels, not macOS windows: mono uppercase
header with context and run id (`KNO / VALUE` … `RUN 0142`), hairline
rules, square corners, no traffic-light dots. Semantic coloring only for
data values. Horizontal scroll inside the panel on small screens — the
page never scrolls horizontally.

Kno output is a primary visual asset: valuation tables, confidence
intervals, asset names, rankings, destinations, keep/reject states appear
throughout the site. A visitor should understand Kno from the outputs
before reading long descriptions.

## 8. Data visualization

Restrained, evidence-bearing, never decorative:

- contribution bars (text-drawn, mono)
- confidence intervals (`──────────●──────`)
- before/after panels (WITHOUT KNO / WITH KNO)
- stage progression (`01 BASELINE ↓ 02 VALUE …`)
- real CLI output verbatim

Illustrative numbers must be labeled illustrative; real output must be
quoted without alteration.

## 9. Buttons and links

- Primary: solid black (white on dark), square, with a directional notch
  on the right edge derived from the mark. Decisive, physical.
- Secondary: outlined, inverts to filled on hover.
- Tertiary: text link with arrow (`READ THE DOCS →`).
- Exactly these three styles. Focus states: 2px outline, offset, inverted
  on dark grounds.

## 10. Iconography

No generic icon library as the dominant language. Where icons are needed,
simple geometric line icons with consistent stroke. For common controls
(GitHub, external link, copy, menu) use recognizable conventions — do not
reinvent usability.

## 11. Motion

Fast, precise, functional, reversible, reduced-motion aware (120–250ms).

- Measurement reveal: sections fade/settle as they enter the viewport
  (`.reveal` + IntersectionObserver; content fully visible without JS).
- No parallax for its own sake, no particles, no cinematic sequences.

## 12. Accessibility

WCAG 2.2 AA minimum: contrast-checked pairings, visible focus, keyboard
navigation (native `details` menus and accordions), semantic tables,
labels beyond color for positive/negative states (`+18.4% POSITIVE`),
reduced-motion overrides, alt text, no information by color alone.

## 13. Editorial texture

Small technical metadata throughout, used selectively:

```
[07 / QUICKSTART]     KNO / OPEN SOURCE     LICENSE / APACHE-2.0
EXPERIMENT / BASELINE     STATUS / SHIPPED
```

These create instrumentation texture — do not over-apply.

## 14. Correct usage examples

- Oversized white symbol cropped in a black hero, headlined in display
  type, with a real terminal panel below.
- `[03 / EVIDENCE]` section head over verbatim CLI output.
- Numbered outcome rows with mono metric blocks on the right.

## 15. Patterns to avoid

- Purple/blue AI gradients, glowing orbs, nebulas, blobs, sparkles,
  glassmorphism, generic grids, circuit boards, fake chat UIs, stock
  photography, robots, brains, neural-net art.
- Rounded cards, pills, drop shadows, glass.
- Generic SaaS layout (`heading → paragraph → three cards`).
- Decorative charts with meaningless data.
- Fake testimonials, fake logos, fabricated adoption metrics.
- Dark mode for the entire site (alternate black sections, keep the
  instrument on a light ground).

## 16. The creative test

Remove the logo from any page. Does it still feel like Kno? It should:
monochrome dominance, mass + measurement, large editorial type, fine
technical annotations, directional geometry, terminal output, experiment
vocabulary, statistical evidence, controlled accent color.

The reference qualities are Stripe's hierarchy, Linear's precision,
Vercel's restraint, GitHub's clarity, a Bloomberg Terminal's density,
scientific-journal typography — as qualities, never as templates.

Kno looks like **the instrument that tells you what your AI actually
knows because of its data** — not a company convincing people it uses AI.
