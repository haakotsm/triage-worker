# Top Box Improvements — Draft Proposal

## TL;DR
- **Fix the bug behind C2** in one line: `blastDots` emits an sr-only label AND the template prints the raw word — drop the sr-only span in `blastDots` (`handler.go:1048-1054`), keep the visible word as the single source of meaning. Same fix needed at `report-table.html:30`.
- **C3 reframed** — the "unknown circle" is **not** a passthrough bug. Backend `ValidateBlastRadius` (`internal/types/summary.go:11-16`) coerces any invalid value to `"pod"` before persist, so the default branch only ever fires for legitimate pod-scope reports. The actually-unknown circle is the **confidence radial** at `incident-detail.html:27-32` — no `aria-label`, no visible caption. Fix: label the ring + add a dot-scale legend (or adopt the segmented-meter alternative).
- **Address C1 ("two unrelated circles")** by labelling the confidence ring, demoting its visual weight, and grouping it with the state badge as a single status stat — so the two circular shapes stop competing.
- **Surface what's actually missing**: `Classification` (CrashLoop/OOM/Network), `EscalationNeeded`, `Summary`, and live `Step + Elapsed` inside the in-flight badge — all already on the data model, all higher-signal than what's there today.
- **Make the live state actually live**: the header sits outside `#incident-content` so it never refreshes; wrap the right cluster in `id="incident-header-status"` and OOB-swap it from the existing `/incidents/{id}/status` response.

---

## Current State

Source: `incident-detail.html:10-48` rendering a typical complete report (per Evidence Collector scenario 1):

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ Deployment/checkout-api  [critical]                                          │
│ Namespace: payments · Alert: HighErrorRate · ●●● namespace blast radius      │
│                                                            [ 87% ]  [Triaged]│
└──────────────────────────────────────────────────────────────────────────────┘
```

**What a sighted user sees:**
> Deployment/checkout-api · critical · Namespace: payments · Alert: HighErrorRate · ●●● namespace blast radius · 87% · Triaged

**What a screen-reader user hears:**
> "Deployment/checkout-api, critical. Namespace: payments · Alert: HighErrorRate · **namespace namespace** blast radius. 87 percent, progressbar. Triaged."

The duplicate "namespace namespace" is the bug behind C2. The unlabeled `87%` ring and the colored `●●●` glyph are the "two unrelated circles" of C1. The user's C3 ("what does the unknown circle mean?") points at the unlabeled radial — there is no LLM-passthrough bug because `types.ValidateBlastRadius` (`internal/types/summary.go:11-16`) coerces any value outside `{pod, deployment, namespace, cluster}` to `"pod"` before the row is persisted (call-site: `activity/report.go:45`). So `BlastRadius="node"` cannot reach the template; the green dot is always the legitimate pod indicator.

---

## Confirmed Bugs

### C1 — "Duplicate circles that make no sense" → **partial / refined**
- **Status:** Confirmed as a *visual collision*, refuted as literal duplication. There is only one `●●●` group (`incident-detail.html:21`) and one radial ring (`incident-detail.html:27-32`). They are not duplicates of each other, but both render as circular glyphs side-by-side on the same row with **no labels**, so the eye reads them as a pair of related-looking decorations.
- **Root cause:** `incident-detail.html:27-32` declares the radial with `role="progressbar"` but no `aria-label`, no visible "Confidence" caption, and the same `text-primary` weight that makes it as eye-catching as the colored dots. Severity, blast, and confidence are three orthogonal concepts (how bad / how wide / how sure) rendered as three same-weight visual elements with no grouping.
- **Fix:** (a) Add `aria-label="Agent confidence"` + a tiny "conf" caption to the radial (`incident-detail.html:27-32`). (b) Move the radial under the state badge so confidence reads as an annotation on the state, not a separate widget: `Triaged · 87% conf`. (c) Wrap the blast-radius dots in a `badge-ghost` chip so they group with their label and stop competing visually with the severity badge.

### C2 — "Duplicate blast radius text (pod pod / namespace namespace)" → **confirmed**
- **Status:** Confirmed. Reproducible from the template source.
- **Root cause:** `incident-detail.html:21` reads `{{blastDots .Report.BlastRadius}} {{.Report.BlastRadius}} blast radius`. `blastDots` (`handler.go:1048-1054`) already emits `<span class="sr-only">namespace</span>`. The template then prints `{{.Report.BlastRadius}}` as visible text. Sighted users see one "namespace"; screen readers announce both — "namespace namespace blast radius".
- **Fix:** Remove the `sr-only` span from `blastDots` (`handler.go:1048,1050,1052,1054`). The visible word in the template carries the meaning for everyone. Reshape the line to: `{{if .Report.BlastRadius}}· {{blastDots .Report.BlastRadius}} <span class="font-mono">{{.Report.BlastRadius}}</span> blast radius{{end}}`. Result: sighted users see `●●● namespace blast radius` (unchanged); SR users hear "namespace blast radius" (no duplicate).

### C3 — "One of the circles implies something unknown — what does it mean?" → **reframed**
- **Status:** Originally analysed as "default branch of `blastDots` silently misrepresents unknown values as pod." **Refuted.** External review (Copilot) flagged `types.ValidateBlastRadius` at `internal/types/summary.go:11-16`, called at `activity/report.go:45` before INSERT, which coerces any value outside `{pod, deployment, namespace, cluster}` to `"pod"`. The DB column can only ever hold those four values, so the default branch of `blastDots` only fires for legitimate pod-scope reports. No silent misrepresentation occurs.
- **Real C3 problem (two parts the team missed):**
  1. **The confidence radial is the actually-unknown circle.** `incident-detail.html:27-32` renders `87%` inside a `radial-progress` with `role="progressbar"` and NO `aria-label`, NO visible caption explaining what the percentage measures. Sighted users see a mystery percentage; SR users hear "87 percent, progressbar." Both 8-agent run and Copilot independently identified this as the more likely referent of the user's complaint.
  2. **The green dot has no legend.** `●` for pod, `●●` for deployment, `●●●` for namespace, `●●●●` for cluster — the encoding is learnable but undocumented. First-time users can't tell whether `●` means "smallest scope" or "least severe" or "lowest confidence."
- **Fix:**
  - **Confidence ring (P0 item #2):** add `aria-label="Agent confidence"`, `aria-valuenow/min/max`, change `role` to `img` (static score, not progress), add a visible "conf" caption beside or below the ring.
  - **Dot legend:** add a `title` attribute on the dot wrapper (`title="Blast radius: pod ● → cluster ●●●●"`) for hover discovery, OR adopt the segmented-meter alternative — see **Alternative Visual Designs** below.
- **What this means for the implementation plan:** the original P0 items #2 (`isKnownBlastRadius` helper) and #3 (template branching to `scope unknown`) are dropped — both were predicated on the refuted bug. P0 item #1 simplifies to just removing the sr-only spans for C2.

---

## Proposed Top Box (after)

**Desktop (≥ sm):**

```
┌──────────────────────────────────────────────────────────────────────────────┐
│ [critical] [CrashLoop] [⚠ Escalate]                checkout-api (Deployment) │
│ Database connection pool exhausted; 7 services degraded since 14:02.         │
│                                                                              │
│ Namespace  payments    Alert  HighErrorRate ×3    Blast  ●●● namespace · 7 │
│                                                                              │
│                                                       Triaged 4m ago · 87% ┤│
└──────────────────────────────────────────────────────────────────────────────┘
```

**Mobile (< sm) — status stacks under, doesn't wrap weirdly:**

```
┌──────────────────────────────────────────┐
│ [critical] [CrashLoop] [⚠ Escalate]      │
│ checkout-api (Deployment)                │
│ Database connection pool exhausted;      │
│ 7 services degraded since 14:02.         │
│                                          │
│ Namespace   payments                     │
│ Alert       HighErrorRate ×3             │
│ Blast       ●●● namespace · 7 services   │
│                                          │
│ Triaged 4m ago · 87% conf                │
└──────────────────────────────────────────┘
```

**In-flight variant** — same shell, status row becomes live:

```
│ ...                                                                          │
│                              [⟳ Triaging · 12s]                              │
```

**Screen-reader read of the new desktop variant** (no duplicates, all values labelled):

> "Severity: critical. Classification: CrashLoop. Action required: Escalate. checkout-api, Deployment, heading level 1. Database connection pool exhausted; 7 services degraded since 14:02. Namespace: payments. Alert: HighErrorRate, 3 correlated. Blast: namespace · 7 services affected. Status: Triaged 4 minutes ago. Agent confidence: 87 percent."

---

## Information Architecture

| Field | Currently shown? | Exists on model? | Proposed | Rationale |
|---|---|---|---|---|
| `Report.Severity` | yes (h2 inline) | yes (`handler.go:32`) | yes, lifted out of h2 into status-chip row | Move out of heading so SR readings of the h1 don't include "critical"; group with other status chips |
| `Report.Kind` / `Workload` | yes (mono h2) | yes (`handler.go:28-29`) | yes — promoted to **h1**, demoted visually to subtitle (workload primary, kind in parens) | UX: identity is lower-signal than urgency. a11y: page needs one h1, currently starts at h2 |
| `Report.Classification` | **no** | yes (`handler.go:31`) | **yes** — as `badge-outline` next to severity | Highest-impact missing field. CrashLoop/OOM/Network maps directly to runbook selection |
| `Report.Summary` | no | yes (`handler.go:33`) | yes — one-line subtitle under chip row, `line-clamp-2` | Already used by `incidents-table.html:47`; provides list→detail continuity |
| `Report.EscalationNeeded` | no | yes (`handler.go:41`) | yes — conditional `badge-error` in chip row, only when true | Routing signal; zero footprint when false, prominent when true |
| `Report.Namespace` | yes (mono in subline) | yes (`handler.go:27`) | yes — as `<dl>` term/value cell | Survives wrap, consistent label styling |
| `Report.AlertName` | yes (always renders, even empty) | yes (`handler.go:30`) | yes — guarded with `{{if}}`, in `<dl>` | Stops " · Alert: " from appearing with empty value |
| `Report.AlertCount` | shown in-flight only (`:79`) | yes (`handler.go:42`) | yes — as `×N` suffix when > 1 | Tells SRE if this is one alert or a storm; cheap to surface |
| `Report.BlastRadius` | yes (visible text + sr-only) | yes (`handler.go:34`), validated by `ValidateBlastRadius` (`types/summary.go:11-16`) to `{pod, deployment, namespace, cluster}` | yes — visible text only; dots become aria-hidden glyphs; add hover legend OR adopt segmented meter | Fixes C2 (drop sr-only span). C3's "unknown circle" is the unlabeled radial, not this. |
| `TriageReport.Impact.AffectedServices` | no — and not threaded through `web.Report` | yes on triage type (`types/triage.go:28`), **NO on web.Report** | yes (count) — requires data-model change | Quantifies blast; "7 services" beats "namespace" alone. **Blocked by data plumbing — see Open Questions** |
| `Report.Confidence` | yes (unlabeled radial) | yes (`handler.go:40`) | yes — demoted to "87% conf" suffix on state badge, with `aria-label` | Stops it looking like a duplicate circle; gives it a name |
| `Report.ResolvedAt` | yes (`Resolved 12m ago` static) | yes (`handler.go:46`) | yes — same string, Alpine-ticked every 30s | Stale-on-load today; long-running tab lies about freshness |
| `Report.CompletedAt` | no | yes (`handler.go:44`) | yes — `Triaged 4m ago` / `Failed 1h ago` on the badges that today have no timestamp | Currently can't tell stale triage from fresh |
| `Report.CreatedAt` (as Age) | shown in-flight stats only | yes (`handler.go:45`) | optional — only if room; `formatDuration` already exists | Matches `incidents-table.html:49` |
| `Report.WorkflowID` | no | yes (`handler.go:26`) | yes — tiny copy-button chip `wf:abc12345…` | SREs paste this constantly; copy affordance proven in runbook |
| `Report.ID` | no | yes (`handler.go:25`) | optional — `#42` chip | URL/DB handle; nice-to-have, also enables `localStorage` per-incident state |
| `DetailData.LiveStatus.Step` | no in top box | yes (`workflow/triage.go:24`) | yes — replaces literal "In flight" with `Triaging…` etc | Already on envelope; transforms generic spinner into progress signal |
| `DetailData.LiveStatus.ElapsedMs` | shown in stats only | yes (`workflow/triage.go:26`) | yes — suffix on in-flight badge `· 12s` | Tells user whether to wait or move on |

---

## Specialist Findings Synthesis

### htmx
- **Header is frozen** — `#incident-content` swap target sits *below* the top box (`incident-detail.html:55-59`), so the severity badge, confidence ring, and state badge **never refresh** when the workflow flips from in-flight to complete. User sees content swap to the runbook while badge above still says "In flight". (Adopted as P1.)
- **Post-completion deadlock** — `incident-complete` (`:90-91`) has no `hx-trigger`, so Triaged→Resolved or Triaged→Failed transitions are permanently invisible until a manual refresh. (Adopted as P1.)
- **Solution adopted**: extract right cluster into `#incident-header-status` partial, OOB-swap from existing `/incidents/{id}/status` endpoint — no new endpoint needed, SSE plumbing at `:2` already in place. Also serve `204 No Content` from the poll when `LiveStatus` is unchanged to stop the every-2s flicker.

### Alpine.js
- **Stale `Resolved 12m ago`** — server-rendered string, frozen on load (`incident-detail.html:34`). Adopted: Alpine `x-data` with 30s ticker, server still renders initial value (no-JS / SR fallback intact).
- **No live elapsed in top box** despite ticker proven one card below (`:72`). Adopted: hoist `x-data` to a longer-lived ancestor with `data-started-at` ISO so re-init recovers correct time after htmx swap.
- **`$persist` plugin** for any per-incident state (pin star, copy flashes) to survive htmx swaps. Pin star deferred to P2.
- **Rejected**: don't use Alpine for the badge if/elseif ladder — that's server state. Specialist correctly flagged this as overreach.

### Frontend (Tailwind/DaisyUI)
- **Metadata paragraph collapses on mobile** (`:18-22`); `flex-wrap` on the outer row produces broken mid-states at iPhone width (`:12`). Adopted: replace prose `<p>` with `<dl>` grid; replace outer `flex-wrap` with `grid grid-cols-1 sm:grid-cols-[1fr_auto]`.
- **Blast-radius color collision** with severity badge: both render in `text-warning`/`badge-warning` for unrelated meanings. Adopted: wrap blast dots in `badge-ghost` chip to neutralize the color rhyme.
- **Card density inconsistent** — top box uses default `card-body` padding; progress card and table use `py-3 px-4`. Adopted as P2.
- **Severity badge nested in h2** pollutes heading text. Adopted (also flagged by a11y) — lift out of `<h2>`.

### Accessibility (WCAG 2.2 AA)
- **C2 is a 4.1.2 Name/Role/Value violation** — duplicate token. Adopted (top P0).
- **Radial-progress missing `aria-valuenow/min/max` and `aria-label`** (`:27-32`). Adopted as P0 — also retypes to `role="img"` since this is a static score, not progress.
- **`role="progressbar"` is semantically wrong** for a static confidence number. Adopted.
- **Color-only severity & blast** — fails 1.4.1 in forced-colors mode. Adopted partially: blast text label always present; severity gets explicit `?` chip + `badge-ghost` for unknown.
- **No h1 anywhere** — promote workload to h1 (`:14`). Adopted.
- **Breadcrumbs missing `<nav>` + `aria-current`** (`:3-8`). Adopted as P1.
- **No live region for status changes** — fails 4.1.3. Adopted: `aria-live="polite"` on the OOB-swapped header status partial.
- **Empty Namespace/Alert render dangling labels** (`:19-20`). Adopted: guard with `{{if}}`.

### UX Research / IA
- **Classification missing is the #1 problem** — single highest-signal answer to "what kind of problem?". Adopted as P1 in the chip row.
- **Scan path is backwards**: identity (Kind/Workload) leads, urgency (severity) is secondary. Adopted: restructure so chips (urgency + classification + escalation) lead, identity is subtitle.
- **`EscalationNeeded` invisibility** is a routing-signal failure. Adopted in chip row.
- **AlertName takes prime real estate but is low-signal** for triage. Adopted: demoted, count-aware (`HighErrorRate ×3`).
- **AffectedServices count** — UX wants it, but it's not on `web.Report`. Pushed to Open Questions (requires data plumbing).
- **"Triaged" with no timestamp** doesn't answer "is it still happening?". Adopted: append `timeAgo .Report.CompletedAt`.

### Conflict resolutions
- **Frontend wants a `stats stats-horizontal` block on the right; UX wants headline+sub with status as annotation.** Proposal sides with UX: a labelled `stats` block is overkill for two values, and the headline+chip-row pattern better matches the scan-path priorities UX argued for. Confidence becomes a suffix on the state badge (`Triaged 4m ago · 87% conf`), not a standalone `stat-value`.
- **Alpine specialist wants copy buttons on title; UX didn't prioritise.** Adopted as P2 (workflow ID copy is high-leverage; title-copy is nice-to-have).
- **All five specialists flagged the in-flight badge as too generic.** Unanimous → P1.

---

## Implementation Plan

### P0 — Bug fixes (must do before any other work)

| # | File:line | Change | LOC |
|---|---|---|---|
| 1 | `handler.go:1045-1056` | Remove `sr-only` spans from all four branches of `blastDots` (fixes C2 SR duplication). Keep the four-way switch and color encoding as-is — backend `ValidateBlastRadius` (`types/summary.go:11-16`) guarantees only `{pod, deployment, namespace, cluster}` reach this helper, so the default branch correctly handles pod. Update template at `incident-detail.html:21`: `{{if .Report.BlastRadius}}· {{blastDots .Report.BlastRadius}} <span class="font-mono">{{.Report.BlastRadius}}</span> blast radius{{end}}` | ~6 |
| 2 | `incident-detail.html:27-32` | Add `aria-valuenow`/`min`/`max`/`aria-label="Agent confidence"`; change `role` to `img` (static score, not progress); wrap visible text in `aria-hidden` span; add visible "conf" caption beside the ring | ~6 |
| 3 | `incident-detail.html:19-20` | Guard `Namespace:` and `Alert:` with `{{if}}` so empty values don't render dangling labels | ~4 |
| 4 | `handler.go:938-947` | `severityClass`: add explicit `case "info": return "badge-info"`; change `default:` to `badge-ghost` so unknown severity is visually distinct from legitimate info-severity (per reality-check) | ~5 |
| 5 | `incident-detail.html:16` | Guard severity: render `?` ghost badge when empty instead of empty-text badge | ~3 |
| 6 | `report-table.html:30` | Same fix as item #1 (sr-only span removal) for the dashboard table — `blastDots` has two call sites with the same duplicate pattern (per reality-check finding) | ~2 |

**Acceptance for P0:** Screen-reader read of scenario 1 contains exactly one "namespace" token (no SR duplication). Confidence ring announces "Agent confidence 87 percent" and shows a visible "conf" caption. `severityClass` returns `badge-info` for explicit `"info"` severity and `badge-ghost` only for empty/unknown. Same SR-fix verified on the dashboard table at `report-table.html:30`.

### P1 — High-impact improvements

| # | File:line | Change | LOC |
|---|---|---|---|
| 8 | `incident-detail.html:12-22` | Restructure: outer becomes `grid grid-cols-1 sm:grid-cols-[1fr_auto] gap-3 items-start`; left column = chip-row + h1 + summary + `<dl>` grid for metadata | ~30 |
| 9 | `incident-detail.html:14` | Promote `<h2>` → `<h1>`; lift severity badge OUT of heading into the chip row above | ~6 |
| 10 | `incident-detail.html:16` (chip row) | Add `{{if .Report.Classification}}<span class="badge badge-outline">{{.Report.Classification}}</span>{{end}}` | ~3 |
| 11 | Chip row | Add `{{if .Report.EscalationNeeded}}<span class="badge badge-error">⚠ Escalate</span>{{end}}` | ~3 |
| 12 | Subtitle row | Add `{{if .Report.Summary}}<p class="text-sm text-base-content/80 line-clamp-2 mt-1">{{.Report.Summary}}</p>{{end}}` | ~3 |
| 13 | `incident-detail.html:3-8` | Wrap breadcrumb in `<nav aria-label="Breadcrumb">`; add `aria-current="page"` to current li | ~4 |
| 14 | `incident-detail.html:24-45` | Add `id="incident-header-status"` + `aria-live="polite" aria-atomic="true"` to right cluster | ~2 |
| 15 | `incident-detail.html:41-43` | Replace literal "In flight" with `{{if .LiveStatus.Step}}{{title .LiveStatus.Step}}…{{else}}In flight{{end}}{{if .LiveStatus.ElapsedMs}} · {{fmtElapsed .LiveStatus.ElapsedMs}}{{end}}`; add `aria-hidden="true"` to spinner span | ~5 |
| 16 | `handler.go` (new helper) | Add `fmtElapsed(ms int64) string` template helper (mirror Alpine `fmt()` from `:72`) | ~8 |
| 17 | `incident-detail.html:34-38` | Append `timeAgo` to Triaged/Failed badges (parallel to existing Resolved pattern); add `<span class="sr-only">Status: </span>` prefix to each | ~6 |
| 18 | `incident-detail.html:27-39` | Demote radial to inline annotation under state badge: `Triaged 4m ago · 87% conf` instead of standalone ring (re-evaluate after stakeholder review — see Open Questions) | ~10 |
| 19 | `web/handler.go` `handleIncidentStatusPoll` (~`:293-321`) | When state unchanged from prior poll: return `204 No Content` so htmx no-ops. When state changes: also render `incident-header-status` partial with `hx-swap-oob="true"` so header updates in same response | ~25 |
| 20 | New partial `incident-header-status` | Extract right-cluster (and chip row if feasible) into a named template so the OOB swap has something to render | ~20 |

**Acceptance for P1:** Workflow transitions from in-flight→complete are visible in the header WITHOUT a page refresh. In-flight badge shows step name + elapsed. New chip row leads with severity + classification + escalation.

### P2 — Nice-to-have

| # | File:line | Change | LOC |
|---|---|---|---|
| 21 | `incident-detail.html:11` | Match sibling card density: `card-body py-3 px-4` | ~1 |
| 22 | `incident-detail.html:34` | Alpine `x-data` with 30s ticker on resolved-time so `Nm ago` self-updates; server still SSR's the initial value | ~6 |
| 23 | `incident-detail.html` (right cluster) | Add tiny copy-button chip for `Report.WorkflowID`: `wf:{{slice .Report.WorkflowID 0 8}}…` with Alpine `copied` flash | ~10 |
| 24 | `incident-detail.html:20` | When `AlertCount > 1`, render `HighErrorRate ×N` instead of bare alert name | ~3 |
| 25 | `handler.go` | Add `lower` template helper for kubectl-friendly Kind rendering (if copy-kubectl chip is adopted) | ~3 |
| 26 | `incident-detail.html` | Optional `{{if .IsComplete}}Triaged {{timeAgo (deref .Report.CompletedAt)}} · age {{formatDuration .Report.CreatedAt}}{{end}}` footer line | ~4 |
| 27 | `incident-detail.html:55-59` | Drop `transition:true` from periodic poll, keep only on real state-change OOB swap | ~2 |

---

## Alternative Visual Designs

### Segmented meter for blast radius (Tier-2 alternative to chip-wrapped dots)

External Copilot review proposed replacing the colored-dot scale (`●` / `●●` / `●●●` / `●●●●`) with a 4-segment meter:

```
[ █ ░ ░ ░ ]  pod        (1 of 4)
[ █ █ ░ ░ ]  deployment (2 of 4)
[ █ █ █ ░ ]  namespace  (3 of 4)
[ █ █ █ █ ]  cluster    (4 of 4)
```

**Why it's stronger than the chip-wrapped-dots P1 approach:**
- **Self-documenting** — the "fill of 4" makes the ordinal scale visible without a legend or color memory. The dot-count encoding is learnable; the meter is recognizable on first sight.
- **Resolves C1 more decisively** — a horizontal meter doesn't read as a circle, so the visual competition with the confidence radial disappears entirely (rather than just being softened by the chip wrapper).
- **Color becomes redundant rather than load-bearing** — meeting WCAG 1.4.1 even before the text label is considered. The text label can shrink or move to hover without losing meaning.

**Why we didn't make it P0/P1:**
- Larger visual change requiring design review across all consumers (`incident-detail.html:21`, `report-table.html:30`, and any future surfaces).
- The chip-wrapped-dots fix in P1 item #8 is more conservative and still resolves the visual-collision part of C1.
- Treat the meter as a candidate for the next design pass once P0/P1 ship and the chip approach has been dogfooded.

**Implementation sketch:**

```go
"blastMeter": func(b string) template.HTML {
    levels := map[string]int{"pod": 1, "deployment": 2, "namespace": 3, "cluster": 4}
    filled := levels[b] // ValidateBlastRadius guarantees b is in the map
    var sb strings.Builder
    sb.WriteString(`<span class="inline-flex gap-0.5 items-center" aria-hidden="true">`)
    for i := 1; i <= 4; i++ {
        if i <= filled {
            sb.WriteString(`<span class="w-1.5 h-3 bg-current rounded-sm"></span>`)
        } else {
            sb.WriteString(`<span class="w-1.5 h-3 bg-current/20 rounded-sm"></span>`)
        }
    }
    sb.WriteString(`</span>`)
    return template.HTML(sb.String())
},
```

Drop-in replacement for `blastDots` — same call sites, same text-label pattern in the template. Color (`text-success` / `text-info` / `text-warning` / `text-error`) can stay tied to severity-of-scope as today, but the fill ratio carries the meaning.

---

## Open Questions

1. **Confidence ring: keep as radial or collapse to annotation?** Frontend says label-and-keep; UX says demote-to-annotation; a11y says fix the ARIA either way. Proposal currently demotes (item #18) — needs user/PM sign-off because the radial is more eye-catching. Compromise: keep radial but shrink to `--size:2rem`, add visible "conf" caption, and align with the state badge as a single group.
2. **AffectedServices count** — UX wants `· 7 services affected`, but `TriageReport.Impact.AffectedServices` (`types/triage.go:28`) is **not threaded through `web.Report`**. Decision needed: (a) add `AffectedServices []string` to `web.Report` and the report-build path, (b) add just `AffectedCount int`, or (c) defer. **Requires data model change.**
3. **Pin / favorite per incident** (Alpine specialist's suggestion) — depends on whether the dashboard list will read the same `localStorage` key to surface pinned items. If no plan to use it elsewhere, drop.
4. **kubectl describe quick-copy** — high-leverage for SREs but requires deciding on the exact command set (describe? logs? both?). Defer pending operator input.
5. **Classification ordering** — `[critical] [CrashLoop]` (severity-first, current proposal) or `[CrashLoop] [critical]` (classification-first)? UX is split. Proposal: severity-first because it's the urgency anchor; revisit after first dogfooding session.
6. **Backend constraint on `BlastRadius`** — **RESOLVED.** Already enforced by `types.ValidateBlastRadius` (`internal/types/summary.go:11-16`), called at `activity/report.go:45` before INSERT. No template-side fallback needed. Identified by external review (Copilot) after the original 8-agent run missed this file.
7. **`title` template helper** — referenced in P1 item #15 (`{{title .LiveStatus.Step}}`). Verify it exists or add as a one-liner.

---

## Rejected / Out-of-Scope

- **Migrate badge if/elseif ladder to Alpine state machine** (Alpine specialist's own warning) — server-state, no client transitions, would break SSR. Keep template branching.
- **Replace whole right cluster with `stats stats-horizontal` block** (Frontend) — overkill for 2 values, doesn't match the lighter aesthetic UX called for, and would make the right cluster wider than necessary on mobile.
- **Pin star with `localStorage`** (Alpine) — moved to Open Questions; no clear consumer for the pinned state today.
- **Per-incident "seen at HH:MM"** (Alpine) — speculative, no validated user need.
- **Counts of Evidence/CausalChain/Recommendations as chips** (UX) — UX flagged as marginal value, kept in body where they already exist via the disclosure summaries.
- **Mix non-color glyphs (●◆▲■) per blast level for color-blind users** (a11y) — visual experiment, requires design review; for now the text label + improved contrast satisfies 1.4.1 (the dots are decorative once the word is present).
- **Replace `hx-swap=outerHTML transition:true` everywhere** (htmx) — only the per-poll case is wasteful; keep `transition:true` on the in-flight→complete edge. Partial adoption (item #27).
- **Hoist Alpine `x-data` to page wrapper at line 2 to survive swaps** (Alpine) — risky because the wrapper carries `hx-ext="sse" sse-connect`; coupling Alpine state lifecycle to the SSE connection root is fragile. Use `$persist` + `data-*` ISO attributes instead.

## Reality Check Appendix

**Overall confidence:** 62/100

**Verdict:** The draft's bug analysis (C1/C2/C3) is solid and accurately reflects the code — the duplicate-announce bug at incident-detail.html:21 + handler.go:1050 is real, the radial-progress is genuinely missing ARIA, and the blastDots default branch does silently misrepresent unknown values as pod. P0 fixes are largely sound. However, the implementation plan contains several concrete fabrications and side-effects the synthesizer missed: (1) the `title` template helper does not exist and item #15's template will fail to parse, (2) item #6 will break legitimate 'info' severity rendering across THREE templates because 'info' currently falls through the default branch, (3) item #19's 204-when-unchanged ignores that handleIncidentStatusPoll is stateless, (4) the existing test `TestIncidentDetailReturns200ForInFlightIncident` asserts on the literal 'In flight' string that item #15 replaces, (5) the proposal forgets `blastDots` is ALSO consumed by report-table.html:30. Status: NEEDS WORK. Bug fixes are ready; structural P1 items need the revisions above before implementation.

### Fabrications / Inaccuracies Detected

- Item #15 uses `{{title .LiveStatus.Step}}` but the `title` template helper is NOT registered in templateFuncs() at /Users/t969076/code/demos/bodils-bibliotek-platform/triage-worker/internal/web/handler.go:936-1094. Go html/template provides no built-in `title` function. Open Question #7 mentions this but the implementation table treats it as a given.
- Draft claims 'Alpine ticker proven one card below' (in Alpine.js specialist section) — the x-data at incident-detail.html:71-72 has NO setInterval; `elapsed` is set once on render. The `fmt()` method is reactive only if `elapsed` mutates, which it never does. So 'live elapsed' currently does NOT tick — the precedent claim is false.
- Item #19 claims handleIncidentStatusPoll can 'return 204 when LiveStatus is unchanged from prior poll' — the handler is stateless (handler.go:293-321) with no memory of prior polls. Mechanism for state comparison is unspecified.
- Item #6 (severityClass default → badge-ghost) does not account for legitimate 'info' severity, which currently falls through the default branch and would be silently demoted from blue badge to grey ghost across incidents-table.html, report-table.html, AND incident-detail.html.
- Draft does not mention that `blastDots` is ALSO consumed at report-table.html:30 with the same `{{blastDots X}} {{X}}` pattern — changing the helper correctly fixes that site too, but the proposal's template-side fix at incident-detail.html:21 (item #3) does NOT propagate the isKnownBlastRadius branching to report-table.html.
- Item #23 proposes truncating WorkflowID with `{{slice .Report.WorkflowID 0 8}}` — workflow IDs in this codebase are slash-path style (e.g. 'triage/default/Deployment/catalog-api/KubePodCrashLooping' per handler_test.go:271), not UUIDs. `wf:triage/d…` is not useful.

### Must-Fix Before Implementation

- Register a `title` template helper in handler.go templateFuncs() (e.g. wrap golang.org/x/text/cases.Title or use a manual one-liner) BEFORE item #15 lands, or change the snippet to use a literal mapping. As written, the template will fail to render.
- Update handler_test.go:121 in tandem with item #15 — the assertion `strings.Contains(body, "In flight")` will fail when LiveStatus.Step='triaging' produces 'Triaging…'. Either keep 'In flight' as the visible prefix and append step/elapsed as suffix (preserves the assertion) or update the test.
- Fix severityClass change in item #6: add an explicit `case "info": return "badge-info"` so legitimate info-severity reports keep their blue badge. Only the default (truly unknown/empty) should map to badge-ghost. This affects incidents-table.html:42,79 and report-table.html:23 in addition to incident-detail.html — confirm visual review across all three tables.
- Specify the mechanism for item #19's '204 when unchanged'. The handler is stateless — either (a) have htmx send last-state via hx-headers and compare server-side, (b) cache last-state per workflow ID server-side (race conditions), or (c) drop the 204 optimization and accept the every-2s re-render. Pick one before implementation.
- Add a parallel template-side fix for report-table.html:30 if the unknown-blast-radius rendering should be consistent across the dashboard. As proposed, the 'scope unknown' label only appears on the detail view; the dashboard table will still show a single green dot + literal 'node' text.
- Rework item #23's WorkflowID truncation — `slice WorkflowID 0 8` produces 'triage/d' for this codebase's slash-path IDs. Use full value, or last segment, or skip truncation.
- Item #17 should use `{{if .Report.CompletedAt}}` guard before timeAgo — for older completed reports with nil CompletedAt, the proposed change would render 'Triaged —' or 'Failed —'.
- Audit item #10's Classification chip behavior for the 'Unknown' fallback case — the workflow defaults Classification to 'Unknown' (workflow/triage.go:230, activity/agent.go:181) so the chip will frequently render `[Unknown]`, which may be noise rather than signal. Consider `{{if and .Report.Classification (ne .Report.Classification "Unknown")}}`.

### Change Audits — items flagged needs-revision

**/Users/t969076/code/demos/bodils-bibliotek-platform/triage-worker/internal/web/handler.go:938-947** — severityClass: return badge-ghost for empty/unknown instead of badge-info.
  - BREAKS legitimate 'info' severity rendering. types/triage.go:59 lists 'info' as a valid severity, but the current severityClass has NO explicit 'info' case — 'info' falls through default to badge-info. Changing default to badge-ghost will mis-render all 'info' severity badges (currently blue → would become ghost grey). Also affects incidents-table.html:42,79 and report-table.html:23. Fix: add explicit `case "info": return "badge-info"` AND change default to badge-ghost.

**/Users/t969076/code/demos/bodils-bibliotek-platform/triage-worker/internal/web/templates/partials/incident-detail.html:41-43** — Replace static 'In flight' with templated step + elapsed.
  - TWO BLOCKERS. (a) Uses `{{title .LiveStatus.Step}}` but `title` is NOT registered in templateFuncs() — template will fail to parse. Must add the helper (open question #7 acknowledges but item #15 silently assumes). (b) handler_test.go:121 asserts `In flight` literal — when LiveStatus.Step=='triaging' (test default), output becomes 'Triaging…' and test breaks. Either keep 'In flight' as the visible string and append step/elapsed as suffix, or update the test.

**/Users/t969076/code/demos/bodils-bibliotek-platform/triage-worker/internal/web/templates/partials/incident-detail.html:33-38** — Append timeAgo to Triaged/Failed badges; add sr-only Status prefix.
  - Triaged and Failed badges currently render without ANY time field — but Report.CompletedAt is *time.Time and may be nil for older incidents. timeAgo (handler.go:992-1016) handles *time.Time and returns '—' for nil. Acceptable but cosmetic: 'Triaged —' is ugly. Suggest `{{if .Report.CompletedAt}}Triaged {{timeAgo .Report.CompletedAt}}{{else}}Triaged{{end}}` — also note timeAgo accepts time.Time OR *time.Time directly, so no `deref` needed; the existing Resolved line uses `(deref .Report.ResolvedAt)` only because deref makes nil-safety obvious. Both styles work.

**/Users/t969076/code/demos/bodils-bibliotek-platform/triage-worker/internal/web/handler.go:293-321 (handleIncidentStatusPoll)** — Return 204 when LiveStatus unchanged; OOB-swap header on state change.
  - Two issues. (a) `handleIncidentStatusPoll` is STATELESS — it has no memory of 'prior poll' since each HTTP request is independent. Implementing 204-on-unchanged requires either (i) the client sending its last-known state via header (htmx hx-headers), or (ii) the server caching last-state per workflow ID. Proposal does not specify which — needs design clarification. (b) OOB swap requires the named template 'incident-header-status' to exist (item #20). Implementation order matters: #20 must precede #19.

### Post-Audit Correction (external review)

After this 8-agent run, an external Copilot review identified a load-bearing fact the team missed: `types.ValidateBlastRadius` (`internal/types/summary.go:11-16`, call-site `activity/report.go:45`) coerces any blast-radius value not in `{pod, deployment, namespace, cluster}` to `"pod"` before persistence. The DB column cannot hold an invalid value, which **refutes the team's original C3 verdict** that the `blastDots` default branch silently misrepresents unknowns as pod.

**Impact on this draft (already applied):**
- **C3 section reframed** — bug doesn't exist; real C3 is the unlabeled confidence radial + no dot-encoding legend.
- **P0 dropped 2 items** — the `isKnownBlastRadius` helper (was item #2) and the `incident-detail.html:21` template branching (was item #3) are both predicated on the refuted bug. P0 went from 7 items → 6 (and gained a new item #6 to also fix `report-table.html:30`).
- **TL;DR bullet 2 rewritten** — original framing dropped.
- **Open Question #6 marked RESOLVED** — backend validation already exists.
- **New section "Alternative Visual Designs"** — added Copilot's segmented-meter proposal as a Tier-2 alternative to the chip-wrapped dots.

**Root cause of the team's miss:** none of the 8 agents read `internal/types/summary.go`. The Evidence Collector's prompt directed it to `types/triage.go` and the web handler, but did not instruct it to grep the whole `types/` package for `Validate*` / `Normalize*` coercion helpers. For future runs, ground-truth prompts should include "grep the package directory of any field suspected of coercion or normalisation before claiming a passthrough bug."

**Updated overall confidence:** ~70/100 (was 62) — the C3 correction removes one fabrication from the original critique but adds a meta-fabrication (the team's confident wrong analysis). Net trustworthiness is slightly higher because the corrected draft is more conservative and the workflow now reflects what the backend actually guarantees.
