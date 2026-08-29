import { describe, expect, it } from 'vitest';
import {
  absoluteUrl,
  commandName,
  deltaClass,
  formatDelta,
  normalizePath,
} from './utils';

describe('formatDelta', () => {
  it('prefixes positive deltas with + and keeps four decimals', () => {
    expect(formatDelta(0.4)).toBe('+0.4000');
  });

  it('prefixes negative deltas with -', () => {
    expect(formatDelta(-0.126)).toBe('-0.1260');
  });

  it('renders an exact zero as plain 0', () => {
    expect(formatDelta(0)).toBe('0');
  });
});

describe('deltaClass', () => {
  it('classifies positive, negative, and zero', () => {
    expect(deltaClass(0.01)).toBe('pos');
    expect(deltaClass(-0.01)).toBe('neg');
    expect(deltaClass(0)).toBe('zero');
  });
});

describe('absoluteUrl', () => {
  it('joins a path onto a site origin', () => {
    expect(absoluteUrl('/blog/', 'https://kno.example.com')).toBe(
      'https://kno.example.com/blog/',
    );
  });

  it('falls back to localhost when no site is configured', () => {
    expect(absoluteUrl('/use-cases/', undefined)).toBe(
      'http://localhost:4321/use-cases/',
    );
  });
});

describe('commandName', () => {
  it('returns the first token of a command', () => {
    expect(commandName('kno baseline --evals cases.jsonl')).toBe('kno');
    expect(commandName('  curl -sSfL https://x | sh')).toBe('curl');
  });
});

describe('normalizePath', () => {
  it('strips trailing slashes except at root', () => {
    expect(normalizePath('/blog/')).toBe('/blog');
    expect(normalizePath('/')).toBe('/');
    expect(normalizePath('/blog')).toBe('/blog');
  });
});
