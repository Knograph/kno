// Package openai is the core.Tuner adapter for OpenAI's fine-tuning API.
//
// Ships second among the bridge's Tuner adapters — see
// docs/plans/2026-09-02-openai-tuner.md — as the deliberate MIRROR IMAGE of
// adapters/tuner/together: OpenAI auto-serves a finished job (no separate
// deploy step, no per-minute hosting bill) but bills inference per token,
// where Together's dedicated endpoint is reserved capacity billed per
// minute with zero-marginal inference. Building a second, structurally
// different Tuner is the entire point of the coretest conformance suite
// this PR also ships — see coretest.ConformTuner.
//
// PROVENANCE WARNING: every request and response shape in this file is
// hand-authored from OpenAI's published fine-tuning API documentation, the
// same discipline together.go's own warning describes ("hand-authored
// first, from the published spec... getting them from the spec and marking
// them (verify) is honest and costs nothing"). Fields marked (verify) below
// were NOT checked against a live call in this PR and may need correction
// on first live use — the design is deliberately arranged so a wrong field
// costs a fixture re-record, not a redesign. See this PR's report for the
// full list of what is and is not confirmed.
//
// docs.md#162's disposition lives at cli/bridge_measure.go's
// confirmAndRun: adapters/agent/pricing's fineTunedTable SHIPS EMPTY for
// "openai" in this PR — the published fine-tuned inference rate is real and
// look-up-able, but sourcing it is a separate, reviewed diff through
// internal/cmd/pricingcheck, not a number hand-typed into this PR. A
// kno bridge run against this adapter under a cost cap REFUSES rather than
// silently treating the eval pass as free — see this package's own doc on
// why that refusal is correct and load-bearing, not a gap.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/knograph/kno/core"
	"github.com/knograph/kno/core/errs"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Public constants a caller needs to configure this adapter.
const (
	// Scheme is the agent-ref scheme this Tuner's models are named under —
	// core.Endpoint.Provider and core.JobRef.Provider both carry it.
	Scheme = "openai"

	// DefaultBaseURL is OpenAI's API root.
	DefaultBaseURL = "https://api.openai.com"

	// DefaultKeyEnv names the environment variable holding the credential
	// for DefaultBaseURL's host, and ONLY for that host — see
	// together.DefaultKeyEnv's doc for the same reasoning, applied here.
	DefaultKeyEnv = "OPENAI_API_KEY"

	// jobsPath is the fine-tuning jobs collection (verify — OpenAI's exact
	// path at the time of writing).
	jobsPath = "/v1/fine_tuning/jobs"

	// modelsPath is the models collection. GET modelsPath/{id} is a free,
	// read-only metadata call — never billed — used by Deploy's readiness
	// probe (see Deploy's own doc for why a probe is needed at all).
	modelsPath = "/v1/models"

	// metadataSuffixKey is the namespaced metadata key Submit tags every
	// job with, and ListJobs filters on — see §6 of
	// docs/plans/2026-09-02-openai-tuner.md: OpenAI's fine-tuning job
	// object echoes no "suffix" field of its own (it appears only inside
	// fine_tuned_model, which is null until the job succeeds — precisely
	// the non-terminal crash state ListJobs exists to recover from). List
	// Jobs supports after, limit, and metadata[k]=v only.
	//
	// Namespaced (edge case 4 of the plan) so a user's own metadata key
	// cannot collide with this adapter's adopt-by-suffix mechanism.
	metadataSuffixKey = "kno_suffix"
)

// Options configure a Tuner. Deliberately plain Go types — see
// together.Options's doc for why: KeyBindings and a Destination would make
// this package unusable from cli, api, and tui.
type Options struct {
	// BaseURL is the endpoint root. Empty uses DefaultBaseURL.
	BaseURL string

	// KeyEnv binds a host to the NAME of the environment variable holding
	// its credential. See together.Options.KeyEnv.
	KeyEnv map[string]string

	// AllowInsecureBaseURL permits a plain-HTTP base URL.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 destinations.
	AllowPrivateAddress bool

	// Timeout bounds a single request. Zero uses the transport default.
	Timeout time.Duration

	// UserAgent identifies Kno to the provider.
	UserAgent string

	// HTTPClient supplies the underlying client's TRANSPORT and TLS
	// settings only — see together.Options.HTTPClient.
	HTTPClient *http.Client

	// PollInterval is the default wait between Status polls, for a caller
	// that wants this adapter's opinion rather than its own. Informational
	// only — this package does not poll on its own; the bridge's
	// orchestration loop calls Status when it decides to.
	PollInterval time.Duration
}

// Tuner submits and tracks fine-tuning jobs against OpenAI's API. Deploy and
// Teardown are no-ops — see their own docs — because OpenAI auto-serves a
// finished job with no separate hosting resource to create or destroy.
//
// Safe for concurrent use: everything it holds is read-only after New.
type Tuner struct {
	opts    Options
	client  *secureClient
	headers http.Header
}

// ErrAuthentication means OpenAI rejected the credential, or none was
// configured for its host.
//
// Its own sentinel for the same reason together.ErrAuthentication has one:
// a rejected key otherwise fails every job in a bridge run with a message
// naming nothing about the cause.
var ErrAuthentication = &errs.Actionable{
	Code:    "OPENAI_AUTH",
	Message: "OpenAI rejected the credential for this run",
	Fix: "set OPENAI_API_KEY in the environment, or bind a key for this host " +
		"with --key-env host=VAR; no file, profile, or metadata service is read",
	ExitCode: errs.ExitError,
}

// ErrProvider means OpenAI answered with an error this adapter cannot
// otherwise classify.
var ErrProvider = &errs.Actionable{
	Code:     "OPENAI_PROVIDER_ERROR",
	Message:  "OpenAI returned an error",
	ExitCode: errs.ExitError,
}

// New builds a Tuner. Everything refusable is refused here — a startup
// error the user can read — rather than per job, where the same mistake
// would fail every group in a bridge run with the same unexplained error.
func New(opts Options) (*Tuner, error) {
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	host, err := hostOf(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	defaultHost, err := hostOf(DefaultBaseURL)
	if err != nil {
		return nil, err
	}
	key, envVar := resolveKey(host, defaultHost, DefaultKeyEnv, opts.KeyEnv)
	switch {
	case key != "":
		// ok
	case envVar != "":
		return nil, ErrAuthentication.
			WithFix(fmt.Sprintf("export %s; it is bound to %s but is empty", envVar, host)).
			Wrap(fmt.Errorf("openai: no credential for %s", host))
	case normalizeHost(host) == normalizeHost(defaultHost):
		return nil, ErrAuthentication.Wrap(fmt.Errorf("openai: no credential for %s", host))
	}

	client, err := newSecureClient(opts.BaseURL, opts.AllowInsecureBaseURL, opts.AllowPrivateAddress,
		opts.Timeout, opts.HTTPClient)
	if err != nil {
		return nil, errs.ErrInvalidInput.Wrap(err)
	}

	headers := http.Header{"Content-Type": []string{"application/json"}}
	if key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	if opts.UserAgent != "" {
		headers.Set("User-Agent", opts.UserAgent)
	}

	return &Tuner{opts: opts, client: client, headers: headers}, nil
}

// --- wire shapes -----------------------------------------------------------
//
// Every field below is (verify) unless stated otherwise in a comment.

// hyperparameters is submitRequest's training-knob sub-object.
type hyperparameters struct {
	NEpochs int32 `json:"n_epochs,omitempty"`
}

// submitRequest is the POST body for jobsPath.
type submitRequest struct {
	Model string `json:"model"`
	// TrainingFile is the file id from OpenAI's Files API. OpenAI requires
	// an upload step before submission (verify); that step is NOT
	// implemented in this PR — mirroring together.go's own documented gap
	// for the identical reason — so this field is currently left empty
	// rather than populated with something wrong. See this PR's report.
	TrainingFile    string            `json:"training_file"`
	Hyperparameters *hyperparameters  `json:"hyperparameters,omitempty"`
	Suffix          string            `json:"suffix,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// submitResponse is jobsPath's 2xx body.
type submitResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	// CreatedAt is UNIX seconds (verify) — unlike Together's RFC 3339
	// string, matching OpenAI's published shape for this field. Converted
	// to RFC 3339 before it reaches core.JobRef.SubmittedAt, whose doc
	// requires that format.
	CreatedAt int64 `json:"created_at"`
}

// errorDetail is the job object's nested error, present once a job reaches
// JOB_STATUS_FAILED.
type errorDetail struct {
	Message string `json:"message"`
}

// statusResponse is the GET-by-id body.
type statusResponse struct {
	ID             string       `json:"id"`
	Status         string       `json:"status"`
	FineTunedModel string       `json:"fine_tuned_model,omitempty"`
	Error          *errorDetail `json:"error,omitempty"`
	// TrainedTokens is reported (verify) once training completes but is
	// NOT a dollar figure — OpenAI's job status API publishes no cost
	// field the way Together's (unconfirmed) total_price_usd_micros does.
	// core.JobState.ActualCostUsdMicros stays zero for every job this
	// adapter reports on, which is the SAME "provider reports no cost"
	// first-class case together.go's own statusResponse doc names, not an
	// oversight: computing a dollar figure here by multiplying
	// TrainedTokens against a training rate would mix a pricing decision
	// into the adapter layer, which is cli's job (resolveTrainPrice), not
	// this one's.
	TrainedTokens int64 `json:"trained_tokens,omitempty"`
}

// jobListItem is one entry in jobsPath's list response.
type jobListItem struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	CreatedAt int64             `json:"created_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// jobListResponse is jobsPath's GET (list) 2xx body.
type jobListResponse struct {
	Data []jobListItem `json:"data"`
}

// --- core.Tuner --------------------------------------------------------

// Submit sends a tuning job to OpenAI.
//
// The caller (bridge.SubmitGroup) has already authorized this through the
// budget guard and written the write-ahead durable row — this method's only
// job is the HTTP call, exactly once, never retried: see together.go's own
// doc on why a retried submit against a provider with no confirmed
// idempotency key is a second job.
func (t *Tuner) Submit(ctx context.Context, job *core.TuningJob) (*core.JobRef, error) {
	if job == nil {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("openai: Submit requires a job"))
	}
	req := submitRequest{
		Model:  job.GetBaseModel().GetTarget(),
		Suffix: job.GetSuffix(),
		// job.GetTrainingData() is the rendered training file bytes — see
		// TrainingFile's own doc for why it is left empty in this PR.
		//
		// job.GetLoraRank() has no field in the fine-tuning jobs API this
		// adapter targets (verify): OpenAI does not expose a LoRA-rank
		// training parameter here the way Together does. Silently dropped
		// rather than guessed into an unconfirmed field — the same
		// "absence over a wrong answer" discipline together.go's mapStatus
		// follows for an unrecognized status string.
	}
	if job.GetEpochs() > 0 {
		req.Hyperparameters = &hyperparameters{NEpochs: job.GetEpochs()}
	}
	if job.GetSuffix() != "" {
		req.Metadata = map[string]string{metadataSuffixKey: job.GetSuffix()}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshaling the submit request: %w", err)
	}

	resp, err := t.client.do(ctx, http.MethodPost, jobsPath, body, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out submitResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("OpenAI returned a 2xx that is not a fine-tuning job response").
			Wrap(err)
	}
	return &core.JobRef{
		Id:          out.ID,
		Provider:    Scheme,
		SubmittedAt: unixToRFC3339(out.CreatedAt),
	}, nil
}

// Status reports where a submitted job stands.
func (t *Tuner) Status(ctx context.Context, ref *core.JobRef) (*core.JobState, error) {
	if ref.GetId() == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("openai: Status requires a job id"))
	}
	resp, err := t.client.do(ctx, http.MethodGet, jobsPath+"/"+url.PathEscape(ref.GetId()), nil, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out statusResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("OpenAI returned a 2xx that is not a job status response").
			Wrap(err)
	}

	state := &core.JobState{
		Ref:    ref,
		Status: mapStatus(out.Status),
		// ActualCostUsdMicros stays zero — see statusResponse.TrainedTokens's
		// doc for why that is the honest value here, not an oversight.
	}
	if out.Error != nil {
		state.Error = out.Error.Message
	}
	if out.FineTunedModel != "" {
		state.TunedModel = &knov1.AgentRef{
			Ref:    Scheme + ":" + out.FineTunedModel,
			Scheme: Scheme,
			Target: out.FineTunedModel,
		}
	}
	return state, nil
}

// Model returns the tuned model as an agent ref. NOT a promise it is
// reachable — see core.Tuner.Model's doc and this package's Deploy: OpenAI
// auto-serves a finished job, but Deploy still confirms the model actually
// answers before reporting ready (see Deploy's own doc for why).
func (t *Tuner) Model(ctx context.Context, ref *core.JobRef) (*core.AgentRef, error) {
	state, err := t.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state.GetTunedModel() == nil {
		return nil, errs.ErrInvalidInput.
			WithFix("wait for the job to reach JOB_STATUS_SUCCEEDED before asking for its model").
			Wrap(fmt.Errorf("openai: job %s has not produced a model yet", ref.GetId()))
	}
	return state.GetTunedModel(), nil
}

// Deploy is a NO-OP that stamps ReadyAt — the no-op core.Tuner.Deploy's own
// doc sanctions for "a provider whose provider auto-serves a finished job":
// OpenAI creates no separate hosting resource, so there is nothing to
// stand up. It is NOT a no-op that skips verification, though.
//
// A finished OpenAI job can reach JOB_STATUS_SUCCEEDED slightly before its
// model is actually invocable — docs/plans/2026-09-02-openai-tuner.md's
// edge case 1: "Deploy must not report ready before the model answers, or
// the eval pass fails against a model that exists but is not callable."
// The naive no-op (`return &core.Endpoint{Ready: true}, nil` off Status
// alone) is exactly the bug #208 and this plan both warn against, doubly
// so here since there is no separate provider call whose own response
// could catch it the way Together's deploy-endpoint response does.
//
// So Deploy makes ONE read-only confirmation call — GET modelsPath/{id},
// which OpenAI does not bill (it is metadata, not inference) — rather than
// an inference probe, which WOULD be a real, unaccounted-for spend outside
// the budget guard (prime directive 4). A 200 means the model is
// registered and, per OpenAI's documented behavior, invocable; a 404 means
// not yet, and Deploy returns an Endpoint with Ready false and a zero
// ReadyAt rather than erroring — bridge.DeployGroup's caller proceeds
// exactly as together.go's own not-yet-ready path would, and a
// premature eval-pass call fails loudly through the normal transport-error
// path (docs/debt.md#161's same reasoning: "money is not at risk, only the
// measurement is").
func (t *Tuner) Deploy(ctx context.Context, ref *core.JobRef) (*core.Endpoint, error) {
	state, err := t.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	model := state.GetTunedModel()
	if model == nil {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("openai: job %s has no model to deploy", ref.GetId()))
	}

	ep := &core.Endpoint{
		// ID: OpenAI creates no separate dedicated-endpoint resource, so
		// there is no provider-assigned id to carry — the fine-tuned
		// model's own name IS what a user would look up in OpenAI's own
		// console, which is exactly what core.Endpoint.ID's doc asks for.
		ID:       model.GetTarget(),
		Provider: Scheme,
		Served:   model.GetTarget(),
		// Replicas: zero. Together's per-minute rate is per replica; a
		// zero-rate no-op Endpoint on an auto-serving provider "may report
		// zero here without implying nothing is servable" — see
		// core.Endpoint.Replicas's own doc, written for this case.
		Replicas: 0,
	}

	ready, err := t.probeReady(ctx, model.GetTarget())
	if err != nil {
		return nil, err
	}
	if ready {
		ep.Ready = true
		ep.ReadyAt = time.Now()
	}
	return ep, nil
}

// probeReady makes the free, read-only GET modelsPath/{id} call Deploy's
// doc describes: 200 means the model answers to a metadata lookup, which
// is OpenAI's documented signal that a fine-tuned model is registered and
// invocable; 404 means not yet. Any other status is a real transport or
// provider failure, propagated rather than silently treated as "not
// ready" — a 5xx here says nothing about the model's own readiness.
func (t *Tuner) probeReady(ctx context.Context, model string) (bool, error) {
	resp, err := t.client.do(ctx, http.MethodGet, modelsPath+"/"+url.PathEscape(model), nil, t.headers)
	if err != nil {
		return false, fromTransport(err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fromStatus(resp.StatusCode, resp.Body)
	}
}

// Teardown is a no-op that CANNOT fail — core.Tuner.Deploy's doc sanctions
// exactly this for an auto-serving provider: OpenAI creates no separate
// hosting resource in Deploy, so there is nothing here to stop, and no HTTP
// call is made at all. Safe to call on every exit path, including when
// Deploy never ran or returned Ready false, matching together.Tuner's own
// contract that Teardown runs unconditionally once Deploy has returned
// successfully.
func (t *Tuner) Teardown(context.Context, *core.Endpoint) error {
	return nil
}

// ListJobs lists OpenAI's fine-tuning jobs tagged with suffix under this
// adapter's namespaced metadata key — the tuner-bridge plan's Step 2(d)
// adopt-by-suffix mechanism, adapted per
// docs/plans/2026-09-02-openai-tuner.md §6: OpenAI's job object echoes no
// "suffix" field, so metadata is what this adapter matches on instead. See
// core.Tuner.ListJobs's doc: the parameter stays named suffix, and what an
// adapter does with it is its own business.
//
// Filtered server-side via metadata[kno_suffix]=<suffix> AND re-checked
// client-side against each returned item's own Metadata — the same
// belt-and-suspenders discipline together.ListJobs applies to its
// (server-unfiltered) suffix match, here guarding against a server or test
// double that accepts the query parameter without honoring it.
func (t *Tuner) ListJobs(ctx context.Context, suffix string) ([]*core.JobRef, error) {
	if suffix == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("openai: ListJobs requires a suffix"))
	}
	q := url.Values{}
	q.Set(fmt.Sprintf("metadata[%s]", metadataSuffixKey), suffix)
	resp, err := t.client.do(ctx, http.MethodGet, jobsPath+"?"+q.Encode(), nil, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out jobListResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("OpenAI returned a 2xx that is not a job list response").
			Wrap(err)
	}

	var refs []*core.JobRef
	for _, item := range out.Data {
		if item.Metadata[metadataSuffixKey] != suffix {
			continue
		}
		refs = append(refs, &core.JobRef{
			Id:          item.ID,
			Provider:    Scheme,
			SubmittedAt: unixToRFC3339(item.CreatedAt),
		})
	}
	// Most-recently-submitted first, matching together.ListJobs's contract.
	sort.Slice(refs, func(i, j int) bool { return refs[i].GetSubmittedAt() > refs[j].GetSubmittedAt() })
	return refs, nil
}

// ListEndpoints ALWAYS returns empty — deliberate, not unimplemented.
//
// core.Tuner.ListEndpoints exists for the tuner-bridge plan's Step 2(g)
// resume-time sweep: a durable row whose Deploy may have succeeded at the
// provider before the row recorded its EndpointID. OpenAI's Deploy (this
// package) creates no provider-side resource at all — it is a read-only
// confirmation probe — so there is never anything for a sweep to find or
// tear down here. Returning empty is the honest answer for "nothing is
// deployed," which is always true for this adapter, per §3 of
// docs/plans/2026-09-02-openai-tuner.md and this package's own doc.
func (t *Tuner) ListEndpoints(_ context.Context, suffix string) ([]*core.Endpoint, error) {
	if suffix == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("openai: ListEndpoints requires a suffix"))
	}
	return nil, nil
}

// mapStatus translates OpenAI's status strings onto JobStatus. (verify —
// OpenAI's exact vocabulary at the time of writing.) An unrecognized string
// maps to UNSPECIFIED rather than guessing, matching together.mapStatus's
// discipline. OpenAI's job status vocabulary has no DEPLOYING analogue —
// Deploy is a separate, adapter-local step (probeReady), not a job-status
// transition — so JOB_STATUS_DEPLOYING is never produced by this function.
func mapStatus(s string) knov1.JobStatus {
	switch strings.ToLower(s) {
	case "validating_files":
		return knov1.JobStatus_JOB_STATUS_VALIDATING_FILES
	case "queued":
		return knov1.JobStatus_JOB_STATUS_QUEUED
	case "running":
		return knov1.JobStatus_JOB_STATUS_RUNNING
	case "succeeded":
		return knov1.JobStatus_JOB_STATUS_SUCCEEDED
	case "failed":
		return knov1.JobStatus_JOB_STATUS_FAILED
	case "cancelled", "canceled":
		return knov1.JobStatus_JOB_STATUS_CANCELLED
	default:
		return knov1.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

// unixToRFC3339 converts OpenAI's unix-seconds created_at into the RFC 3339
// string core.JobRef.SubmittedAt's doc requires — unlike Together, whose
// (verify) created_at is already an RFC 3339 string.
func unixToRFC3339(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

var _ core.Tuner = (*Tuner)(nil)
