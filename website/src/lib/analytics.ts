/**
 * Vendor-neutral analytics abstraction.
 *
 * Components never call a provider directly — they call `track()`. Providers
 * are registered here; the active provider is chosen at build time from
 * `PUBLIC_ANALYTICS_PROVIDER`. With no provider configured (the default),
 * every call is a no-op and the site works exactly as if analytics did not
 * exist. In dev, events are logged to the console instead.
 *
 * Providers:
 *   - "cloudflare": forwards events to Cloudflare Zaraz (`zaraz.track`), so
 *     they can be routed to any Zaraz tool or to the Web Analytics dashboard.
 *     Requires Zaraz to be enabled on the zone. See docs/DEPLOYMENT.md.
 *   - unset: no-op.
 */

export type AnalyticsEventName =
  | 'hero_install_copy'
  | 'hero_github_click'
  | 'hero_get_started'
  | 'quickstart_install_copy'
  | 'quickstart_command_copy'
  | 'docs_click'
  | 'use_case_click'
  | 'contributing_click'
  | 'good_first_issue_click'
  | 'github_star_intent'
  | 'cms_content_contribution_click';

export interface AnalyticsProps {
  /** Pathname of the page the event fired on. */
  page?: string;
  /** Where on the page the element lived, e.g. "hero", "quickstart". */
  location?: string;
  /** The command that was copied, if any. */
  command?: string;
  /** Referrer path, when known. */
  referrer?: string;
  [key: string]: string | undefined;
}

type Provider = (name: AnalyticsEventName, props: AnalyticsProps) => void;

declare global {
  interface Window {
    zaraz?: {
      track: (name: string, props?: Record<string, unknown>) => void;
    };
  }
}

const providers: Record<string, Provider> = {
  cloudflare(name, props) {
    if (typeof window !== 'undefined' && window.zaraz?.track) {
      window.zaraz.track(`kno_${name}`, props);
    }
  },
};

const providerName =
  (import.meta.env.PUBLIC_ANALYTICS_PROVIDER as string | undefined) ?? '';

export function track(name: AnalyticsEventName, props: AnalyticsProps = {}): void {
  if (import.meta.env.DEV) {
    console.debug('[analytics]', name, props);
    return;
  }
  const provider = providers[providerName];
  if (provider) {
    provider(name, props);
  }
}

/** Names of the events the site instruments. Kept in one place so the
 * instrumentation inventory (docs/ANALYTICS.md, tests) cannot drift. */
export const ANALYTICS_EVENTS: readonly AnalyticsEventName[] = [
  'hero_install_copy',
  'hero_github_click',
  'hero_get_started',
  'quickstart_install_copy',
  'quickstart_command_copy',
  'docs_click',
  'use_case_click',
  'contributing_click',
  'good_first_issue_click',
  'github_star_intent',
  'cms_content_contribution_click',
];
