# Analytics instrumentation

The site ships a small, vendor-neutral analytics abstraction
(`src/lib/analytics.ts`). Components never call a provider directly; they
call `track(name, props)` or dispatch a `kno:track` CustomEvent, which
`src/components/Analytics.astro` forwards into the tracker.

**The site works perfectly with analytics disabled** — with no provider
configured, every call is a no-op (console logging in dev only).

## Providers

- **`cloudflare`** — forwards events to Cloudflare Zaraz
  (`zaraz.track('kno_<event>', props)`). Enable Zaraz on the zone, then
  create a trigger matching `kno_*` event names and attach it to any
  tool (Web Analytics, Workers Analytics Engine, or an external
  destination).
- **unset (default)** — no tracking at all.

Set `PUBLIC_ANALYTICS_PROVIDER=cloudflare` in the Pages environment to
enable. No analytics JavaScript loads from third-party domains.

## Instrumented events

Defined once in `ANALYTICS_EVENTS` so the inventory cannot drift:

| Event                            | Fires when                                  |
| -------------------------------- | ------------------------------------------- |
| `hero_install_copy`              | The hero install command is copied          |
| `hero_github_click`              | "View on GitHub" in the hero is clicked     |
| `hero_get_started`               | "Run Kno" is clicked                        |
| `quickstart_install_copy`        | The final-CTA install command is copied     |
| `quickstart_command_copy`        | Any quickstart step command is copied       |
| `docs_click`                     | A docs deep link is clicked                 |
| `use_case_click`                 | A use-case link is clicked (home or index)  |
| `contributing_click`             | A contributing/community link is clicked    |
| `good_first_issue_click`         | The good-first-issues CTA is clicked        |
| `github_star_intent`             | Reserved — fire when a star CTA is added    |
| `cms_content_contribution_click` | The footer "Edit this site" link is clicked |

Properties: `page`, `location` (hero / quickstart-step-N / community /
…), `command` (for copy events), `referrer` when relevant.

## Adding an event

1. Add the name to `AnalyticsEventName` and `ANALYTICS_EVENTS` in
   `src/lib/analytics.ts`.
2. Fire it from the component via `data-analytics-event` (plain
   clicks) or a `kno:track` CustomEvent (copy buttons).
3. Document it in the table above.
