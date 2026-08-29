import { defineCollection, z } from 'astro:content';
import { glob } from 'astro/loaders';

/**
 * Content schema — the contract between the CMS and the renderer.
 *
 * Every field editable in Sveltia CMS is declared here. Schemas are loose by
 * design (optional fields, defaults) so a partially-filled CMS draft never
 * breaks the build.
 */

/* ------------------------------------------------------------------ site */
/** Site-wide settings singleton. */
const siteCollection = defineCollection({
  loader: glob({ pattern: '*.yaml', base: './src/content/site' }),
  schema: z.object({
    title: z.string().default('Kno'),
    shortDescription: z
      .string()
      .default('Know which data actually makes your AI better.'),
    githubUrl: z.string().url().default('https://github.com/uknoAI/kno'),
    docsUrl: z.string().url().default('https://github.com/uknoAI/kno#documentation'),
    installCommand: z
      .string()
      .default(
        'curl -sSfL https://raw.githubusercontent.com/uknoAI/kno/main/install.sh | sh',
      ),
    navigation: z.array(z.object({ label: z.string(), url: z.string() })).default([
      { label: 'Docs', url: '' },
      { label: 'Use Cases', url: '/use-cases/' },
      { label: 'Blog', url: '/blog/' },
      { label: 'Community', url: '/community/' },
      { label: 'GitHub', url: 'https://github.com/uknoAI/kno' },
    ]),
    socialLinks: z
      .array(z.object({ label: z.string(), url: z.string() }))
      .default([{ label: 'GitHub', url: 'https://github.com/uknoAI/kno' }]),
    seoTitle: z.string().default('Kno — know which data actually makes your AI better'),
    seoDescription: z
      .string()
      .default(
        'Open-source agent data evaluation. Measure which documents, examples, and policies actually improve your AI — and what each one costs.',
      ),
    ogImage: z.string().default('/images/og.png'),
    footerLinks: z.array(z.object({ label: z.string(), url: z.string() })).default([
      { label: 'GitHub', url: 'https://github.com/uknoAI/kno' },
      { label: 'Docs', url: 'https://github.com/uknoAI/kno#documentation' },
      { label: 'Apache-2.0', url: 'https://github.com/uknoAI/kno/blob/main/LICENSE' },
      { label: 'Blog', url: '/blog/' },
      { label: 'Edit this site', url: '/admin/' },
    ]),
  }),
});

/* ------------------------------------------------------------------ home */
/** Homepage singleton. */
const homeCollection = defineCollection({
  loader: glob({ pattern: '*.yaml', base: './src/content/home' }),
  schema: z.object({
    hero: z.object({
      eyebrow: z.string().default('Open-source agent data evaluation'),
      headline: z.string().default('Know which data actually makes your AI better.'),
      subheadline: z
        .string()
        .default(
          'Measure which documents, examples, policies, and conversations improve your agent. Rank them by impact and cost, then put each where it belongs.',
        ),
      primaryCtaLabel: z.string().default('Run Kno'),
      primaryCtaUrl: z.string().default('#quickstart'),
      secondaryCtaLabel: z.string().default('View on GitHub'),
      secondaryCtaUrl: z.string().default('https://github.com/uknoAI/kno'),
      installCommand: z
        .string()
        .default(
          'curl -sSfL https://raw.githubusercontent.com/uknoAI/kno/main/install.sh | sh',
        ),
    }),
    trustStrip: z
      .array(z.string())
      .default([
        'Single Go binary',
        'No infrastructure',
        'Works with your existing evals',
        'OpenAI-compatible endpoints',
        'Anthropic',
        'Local agents',
        'Budget guarded',
        'Apache-2.0',
      ]),
    valuation: z.object({
      eyebrow: z.string().default('Proof'),
      heading: z.string().default('Every piece of agent data should earn its place.'),
      intro: z
        .string()
        .default(
          'A valuation answers one question per asset: keep it, drop it, or move it — with the measured delta that justifies the decision.',
        ),
      rows: z
        .array(
          z.object({
            asset: z.string(),
            impact: z.string(),
            decision: z.string(),
          }),
        )
        .default([
          {
            asset: 'new_refund_policy.md',
            impact: '+18%',
            decision: 'keep → knowledge base',
          },
          { asset: 'example_42.json', impact: '+7%', decision: 'keep → context' },
          { asset: 'example_91.json', impact: '+1%', decision: 'reject' },
          { asset: 'old_refund_policy.md', impact: '-9%', decision: 'reject → harmful' },
        ]),
      note: z
        .string()
        .default(
          'Illustrative example. Real runs report each delta with its 95% confidence interval; destinations come from the select stage.',
        ),
    }),
    problem: z.object({
      heading: z
        .string()
        .default("You're measuring your models. Why aren't you measuring your data?"),
      body: z
        .string()
        .default(
          'Agent teams route data constantly — put it in RAG, add it to context, fine-tune on it — with intuition as the decision method. Kno turns each of those decisions into a measured experiment.',
        ),
    }),
    outcomes: z
      .array(
        z.object({
          icon: z.string(), // short monospace identifier, e.g. "+Δ"
          title: z.string(),
          description: z.string(),
        }),
      )
      .default([
        {
          icon: '+Δ',
          title: 'Know what helps',
          description:
            'Measure the marginal impact of each candidate asset against your own evals.',
        },
        {
          icon: '-Δ',
          title: 'Know what hurts',
          description:
            'Find contradictory and harmful data before it reaches production.',
        },
        {
          icon: '$',
          title: 'Know what it costs',
          description:
            'Compare improvement against inference cost — ranking is per dollar, not per point.',
        },
        {
          icon: '→',
          title: 'Know where it belongs',
          description:
            'Context, knowledge base, or tuning set — each asset gets a destination, not just a score.',
        },
        {
          icon: '∅',
          title: "Know what's missing",
          description:
            'Gaps in the information your agent needs show up as cases your data cannot move.',
        },
      ]),
    howItWorks: z.object({
      heading: z.string().default('Five stages. One question each.'),
      intro: z
        .string()
        .default(
          'Kno treats data as an experimental variable. Each stage answers a single question about your agent and the assets you feed it.',
        ),
      stages: z
        .array(
          z.object({
            name: z.string(),
            question: z.string(),
            description: z.string(),
            status: z.enum(['shipped', 'planned']).default('shipped'),
          }),
        )
        .default([
          {
            name: 'Baseline',
            question: 'How good is the agent now?',
            description:
              'Run the agent over your dev cases, score against your goal, persist every result.',
            status: 'shipped',
          },
          {
            name: 'Value',
            question: 'Which assets improve it?',
            description:
              'Route each asset to the slices it could affect, inject it, re-measure against fresh controls, record the delta with an interval.',
            status: 'shipped',
          },
          {
            name: 'Select',
            question: 'Which combination should I keep?',
            description:
              'Build a portfolio under budget with a rejection log; every decision at a Bonferroni-corrected interval.',
            status: 'shipped',
          },
          {
            name: 'Validate',
            question: 'Does the combination still work on untouched evals?',
            description: 'Measure the portfolio as a set against the sealed holdout.',
            status: 'planned',
          },
          {
            name: 'Export',
            question: 'Where should each asset go?',
            description:
              'Render selected assets into the destination grammar: context pack, knowledge-base manifest, or tuning-set JSONL.',
            status: 'shipped',
          },
        ]),
    }),
    comparison: z.object({
      heading: z
        .string()
        .default(
          'Your eval framework measures the agent. Kno measures the data feeding it.',
        ),
      intro: z
        .string()
        .default(
          'Kno does not replace your evals — it uses them. Existing evals tell you whether your agent got better. Kno tells you which asset caused it.',
        ),
      rows: z
        .array(
          z.object({
            capability: z.string(),
            evalFrameworks: z.enum(['yes', 'no', 'uses']),
            kno: z.enum(['yes', 'no', 'uses']),
          }),
        )
        .default([
          { capability: 'Model performance', evalFrameworks: 'yes', kno: 'uses' },
          { capability: 'Prompt performance', evalFrameworks: 'yes', kno: 'uses' },
          { capability: 'Asset-level impact', evalFrameworks: 'no', kno: 'yes' },
          { capability: 'Marginal data value', evalFrameworks: 'no', kno: 'yes' },
          { capability: 'Data cost', evalFrameworks: 'no', kno: 'yes' },
          { capability: 'Destination selection', evalFrameworks: 'no', kno: 'yes' },
          { capability: 'Portfolio validation', evalFrameworks: 'no', kno: 'yes' },
        ]),
    }),
    quickstart: z.object({
      heading: z.string().default('From zero to your first valuation'),
      intro: z
        .string()
        .default(
          'Four steps, no API keys, no spend. The whole loop runs against a local fake agent, so you can see every stage work before pointing Kno at anything that bills you.',
        ),
      steps: z
        .array(
          z.object({
            title: z.string(),
            command: z.string().optional(),
            output: z.string().optional(),
            explanation: z.string(),
          }),
        )
        .default([
          {
            title: 'Write some cases',
            command:
              'printf \'{"id":"refund-01","input":"How do I get a refund?","expected":"Refunds are processed within 5 business days."}\\n\' > cases.jsonl',
            explanation: 'One scoreable interaction per line, each with a stable id.',
          },
          {
            title: 'Measure the agent as it is today',
            command: 'kno baseline --evals cases.jsonl',
            explanation:
              'The reference every later number is compared against. Kno seals a holdout here — nothing reads it until validate.',
          },
          {
            title: 'Write a pool of candidate assets',
            command:
              'printf \'{"id":"refund-policy-v3","content":"Refunds are processed within 5 business days.","kind":"knowledge"}\\n\' > pool.jsonl',
            explanation:
              'The documents, examples, and policies you are considering adding.',
          },
          {
            title: 'Value them',
            command:
              'kno value --evals cases.jsonl --pool pool.jsonl --baseline-run-id <run id>',
            explanation:
              'Each asset is injected into the slices it could affect, re-measured against controls, and reported with a confidence interval.',
          },
        ]),
      docsLabel: z.string().default('Read the full walkthrough'),
      docsUrl: z
        .string()
        .default(
          'https://github.com/uknoAI/kno/blob/main/docs/cookbook/first-baseline.md',
        ),
      freeNote: z
        .string()
        .default(
          'The quickstart costs nothing: the default agent is a local fake that answers every case with what the case expects. It proves the loop — routing, injection, controls, intervals — before real money is involved.',
        ),
    }),
    trust: z.object({
      heading: z.string().default('Questions engineers actually ask'),
      intro: z
        .string()
        .default(
          'Kno is deliberately opinionated about experimentation. The short answers, from the source.',
        ),
      items: z.array(z.object({ question: z.string(), answer: z.string() })).default([
        {
          question: 'Does Kno replace my eval framework?',
          answer:
            'No — it uses it. Your evals score the agent; Kno re-scores it with each candidate asset to measure the marginal change. Existing evals stay the source of truth for quality.',
        },
        {
          question: 'Does Kno send my data somewhere?',
          answer:
            'Runs are stored locally in SQLite — including agent output, which may be conversation content. Kno itself sends nothing anywhere; there is no telemetry of content. Your cases go only to the provider you point Kno at.',
        },
        {
          question: 'What happens to traces?',
          answer:
            'They stay on your disk until you delete them. Nothing expires on its own; `kno purge` removes trace content when you no longer need it, keeping scores and costs so runs stay resumable.',
        },
        {
          question: 'Can it accidentally spend unlimited API money?',
          answer:
            'No. Every path that can call a provider goes through a budget guard: estimate, confirm, checkpoint. Caps are enforced before the call, not discovered at settlement. `--max-cost-usd` with `--yes` makes a run unattended-safe.',
        },
        {
          question: 'Are the results statistically meaningful?',
          answer:
            'Kno reports confidence intervals, never naked point estimates — a delta without its interval is not reported at all. Controls measure regression separately, and selection decisions are made at Bonferroni-corrected intervals.',
        },
        {
          question: 'What providers work?',
          answer:
            '`openai:` for any OpenAI-compatible endpoint (vLLM, Ollama, llama.cpp need no key), `anthropic:` for the Anthropic API, and `fake:` for the free local agent. Keys come from the environment, never from a flag.',
        },
        {
          question: 'Do I need infrastructure?',
          answer:
            'No. Kno is a single Go binary. State is a local SQLite file, work checkpoints as it goes, and interrupted runs resume with `--resume` without paying twice.',
        },
      ]),
    }),
    community: z.object({
      heading: z.string().default('Kno is designed to be extended'),
      description: z
        .string()
        .default(
          'Adapters bring new data sources, judges add evaluation logic, goals add optimization targets, providers connect agent runtimes — and the core loop itself is open. Pick the surface that matches how deep you want to go.',
        ),
      categories: z
        .array(z.object({ title: z.string(), description: z.string() }))
        .default([
          {
            title: 'Adapters',
            description:
              'Bring another data source — the cookbook shows the pattern against Zendesk.',
          },
          {
            title: 'Judges',
            description: 'Add evaluation logic so Kno can score a new kind of case.',
          },
          {
            title: 'Goals',
            description: 'Add an optimization target for the select stage.',
          },
          {
            title: 'Providers',
            description: 'Connect a new agent runtime.',
          },
          {
            title: 'Core',
            description: 'Improve the valuation engine itself.',
          },
        ]),
      ctas: z.array(z.object({ label: z.string(), url: z.string() })).default([
        {
          label: 'Browse good first issues',
          url: 'https://github.com/uknoAI/kno/contribute',
        },
        {
          label: 'Read CONTRIBUTING.md',
          url: 'https://github.com/uknoAI/kno/blob/main/CONTRIBUTING.md',
        },
        {
          label: 'Open an issue',
          url: 'https://github.com/uknoAI/kno/issues/new/choose',
        },
      ]),
    }),
    roadmap: z.object({
      heading: z.string().default('Kno is early. Help define what comes next.'),
      intro: z
        .string()
        .default(
          'The measurement loop ships — baseline, value, select, and export are real. Validate is next. Everything here is transparent, including the debt.',
        ),
      stages: z
        .array(z.object({ name: z.string(), status: z.enum(['shipped', 'planned']) }))
        .default([
          { name: 'baseline', status: 'shipped' },
          { name: 'value', status: 'shipped' },
          { name: 'select', status: 'shipped' },
          { name: 'validate', status: 'planned' },
          { name: 'export', status: 'shipped' },
          { name: 'report', status: 'shipped' },
        ]),
      ctaLabel: z.string().default('See the full status table'),
      ctaUrl: z.string().default('https://github.com/uknoAI/kno#status'),
    }),
    finalCta: z.object({
      heading: z.string().default('Stop guessing which data makes your agent better.'),
      body: z
        .string()
        .default(
          'Install the binary, run the quickstart, and get your first valuation in minutes — without spending API money.',
        ),
      installCommand: z
        .string()
        .default(
          'curl -sSfL https://raw.githubusercontent.com/uknoAI/kno/main/install.sh | sh',
        ),
      ctas: z.array(z.object({ label: z.string(), url: z.string() })).default([
        { label: 'Run the quickstart', url: '#quickstart' },
        {
          label: 'View on GitHub',
          url: 'https://github.com/uknoAI/kno',
        },
      ]),
    }),
  }),
});

/* ------------------------------------------------------------------ blog */
const blogCollection = defineCollection({
  loader: glob({ pattern: '**/*.md', base: './src/content/blog' }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    pubDate: z.coerce.date(),
    updatedDate: z.coerce.date().optional(),
    author: z.string().default('Kno'),
    tags: z.array(z.string()).default([]),
    image: z.string().optional(),
    draft: z.boolean().default(false),
    seoTitle: z.string().optional(),
    seoDescription: z.string().optional(),
  }),
});

/* -------------------------------------------------------------- use cases */
const useCasesCollection = defineCollection({
  loader: glob({ pattern: '**/*.md', base: './src/content/use-cases' }),
  schema: z.object({
    title: z.string(),
    question: z.string(),
    summary: z.string(),
    problem: z.string(),
    workflow: z.array(z.object({ step: z.string(), detail: z.string() })).default([]),
    example: z.string().optional(),
    stages: z.array(z.string()).default([]),
    ctaLabel: z.string().default('Try it'),
    ctaUrl: z.string().default('#quickstart'),
    seoTitle: z.string().optional(),
    seoDescription: z.string().optional(),
  }),
});

/* ----------------------------------------------------------------- pages */
const pagesCollection = defineCollection({
  loader: glob({ pattern: '**/*.md', base: './src/content/pages' }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    order: z.number().default(0),
    seoTitle: z.string().optional(),
    seoDescription: z.string().optional(),
  }),
});

export const collections = {
  site: siteCollection,
  home: homeCollection,
  blog: blogCollection,
  'use-cases': useCasesCollection,
  pages: pagesCollection,
};
