/**
 * Pure helpers shared across pages. Kept dependency-free so they are cheap
 * to unit-test (see src/lib/*.test.ts).
 */

/** Format a numeric delta the way Kno reports it: signed, four decimals.
 * `0` stays "0", so a genuinely-zero delta is not dressed up as a change. */
export function formatDelta(delta: number): string {
  if (delta === 0) return '0';
  const sign = delta > 0 ? '+' : '';
  return `${sign}${delta.toFixed(4)}`;
}

/** Classify a delta for styling: 'pos' | 'neg' | 'zero'. Never the only
 * channel of information — callers must also render the sign/text. */
export function deltaClass(delta: number): 'pos' | 'neg' | 'zero' {
  if (delta > 0) return 'pos';
  if (delta < 0) return 'neg';
  return 'zero';
}

/** Build an absolute URL against the site origin. Falls back to the current
 * origin at runtime so preview deployments keep working. */
export function absoluteUrl(path: string, site: string | URL | undefined): string {
  if (site) {
    const base = typeof site === 'string' ? site : site.toString();
    return new URL(path, base).toString();
  }
  return new URL(path, 'http://localhost:4321').toString();
}

/** Extract the leading language of a command line for display purposes. */
export function commandName(command: string): string {
  return command.trim().split(/\s+/)[0] ?? '';
}

/** Strip a trailing slash from a path without touching the root. */
export function normalizePath(path: string): string {
  if (path.length > 1 && path.endsWith('/')) return path.slice(0, -1);
  return path;
}
