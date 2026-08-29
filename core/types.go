package core

import knov1 "github.com/knograph/kno/gen/kno/v1"

// The kno.v1 generated messages ARE the domain types. These are type ALIASES,
// not defined types, so core.Case and knov1.Case are the same type: there is
// one definition, no conversion layer, and nothing to drift.
//
// The cost, accepted deliberately in ADR-0001, is that these carry generated
// field spellings — Asset.Id, not Asset.ID. Three rules make the design safe,
// and all three are enforced mechanically rather than by convention:
//
//   - Always pass these by POINTER. They carry DoNotCopy; `go vet`'s copylocks
//     catches value copies.
//   - Never compare them with reflect.DeepEqual. They carry DoNotCompare, so
//     comparing two through an `any` is a runtime panic that `go vet` cannot
//     see. forbidigo bans it repo-wide; use protocmp.Transform in tests.
//   - Never marshal them with encoding/json. Proto3 JSON requires int64 as
//     quoted strings and enums as names; encoding/json emits neither, silently
//     diverging from the generated OpenAPI spec. depguard bans it; use
//     protojson.
//
// See docs/adr/0001-proto-as-domain-types.md.
type (
	// Case is one scoreable eval interaction.
	Case = knov1.Case

	// Response is what an Agent returned for a Case.
	Response = knov1.Response

	// Score is a Goal's judgement of one Response.
	Score = knov1.Score

	// Asset is one candidate data unit, carrying its own economics.
	Asset = knov1.Asset

	// Valuation is an Asset's measured record.
	Valuation = knov1.Valuation

	// Portfolio is the selected subset of Assets with its rejection log.
	Portfolio = knov1.Portfolio

	// PortfolioEntry is one selected Asset and the measurement that earned it.
	PortfolioEntry = knov1.PortfolioEntry

	// Rejection is one excluded Asset and why.
	Rejection = knov1.Rejection

	// Interval is a confidence interval, deliberately a message so absence
	// is representable.
	Interval = knov1.Interval

	// Report is the deliverable: what shipped, what didn't, and what it's worth.
	Report = knov1.Report

	// Capabilities is what an adapter can actually do.
	Capabilities = knov1.Capabilities

	// AgentRef identifies an agent or model.
	AgentRef = knov1.AgentRef

	// TuningJob is a fine-tuning run submitted to a hosted provider.
	TuningJob = knov1.TuningJob

	// JobRef identifies a submitted tuning job with its provider.
	JobRef = knov1.JobRef

	// JobState is a tuning job's status as reported by its provider.
	JobState = knov1.JobState

	// Kind is what an Asset fundamentally is: knowledge or behavior.
	Kind = knov1.Kind

	// Destination is where a measured Asset belongs.
	Destination = knov1.Destination

	// Direction is which way is better for a Goal.
	Direction = knov1.Direction

	// ScoreDomain is the set of values a Goal.Score can take. See Goal.Domain.
	ScoreDomain = knov1.ScoreDomain

	// InjectionMode records how a Valuation was measured.
	InjectionMode = knov1.InjectionMode

	// Split says which half of the Evals a Case belongs to.
	Split = knov1.Split
)
