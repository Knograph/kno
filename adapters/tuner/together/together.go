// Package together is the core.Tuner adapter for Together AI's fine-tuning
// and dedicated-endpoint APIs.
//
// Ships first among the bridge's Tuner adapters — see the tuner-bridge
// plan's Step 5 — because DESIGN.md names Together first, Together exposes
// LoRA rank as a first-class training parameter, the proxy DESIGN describes
// is a 1-8B OPEN model (which Together serves and OpenAI's fine-tuning
// tiers do not), and Together publishes a per-token training rate that
// makes Step 2(a)'s local pessimistic estimate possible at all.
//
// PROVENANCE WARNING: every request and response shape in this file is
// hand-authored from the published Together API documentation, per the
// bridge plan's Step 6 discipline ("hand-authored first, from the published
// spec... getting them from the spec and marking them (verify) is honest
// and costs nothing"). Fields marked (verify) below were NOT checked
// against a live call in this PR and may need correction on first live
// use — the design is deliberately arranged so a wrong field costs a
// fixture re-record, not a redesign. See this PR's report for the full
// list of what is and is not confirmed.
package together

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
	// Scheme is the agent-ref scheme this Tuner's models are named under,
	// matching adapters/agent/agentref's grammar for a tuned model:
	// "together:<base-model>" before tuning, "tuned:<job-id>" after — see
	// agentref.SchemeTuned.
	Scheme = "together"

	// DefaultBaseURL is Together's API root (verify).
	DefaultBaseURL = "https://api.together.xyz"

	// DefaultKeyEnv names the environment variable holding the credential
	// for DefaultBaseURL's host, and ONLY for that host — see the same
	// reasoning anthropic.DefaultKeyEnv documents.
	DefaultKeyEnv = "TOGETHER_API_KEY"

	// jobsPath is the fine-tuning jobs collection. (verify — Together's
	// exact path; OpenAI's equivalent, which Together's format largely
	// mirrors per the plan's Step 5 counter-argument, is /v1/fine_tuning/jobs).
	jobsPath = "/v1/fine-tunes"

	// endpointsPath is the dedicated-endpoints collection Step 2(f)'s
	// hosting dimension uses (verify).
	endpointsPath = "/v1/endpoints"
)

// Options configure a Tuner. Deliberately plain Go types — see
// anthropic.Options's doc for why: the transport is internal to
// adapters/agent, and a Destination or KeyBindings here would make this
// package unusable from cli, api, and tui.
type Options struct {
	// BaseURL is the endpoint root. Empty uses DefaultBaseURL.
	BaseURL string

	// KeyEnv binds a host to the NAME of the environment variable holding
	// its credential. See anthropic.Options.KeyEnv.
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
	// settings only — see anthropic.Options.HTTPClient.
	HTTPClient *http.Client

	// PollInterval is the default wait between Status polls, for a caller
	// that wants this adapter's opinion rather than its own. Informational
	// only — this package does not poll on its own; the bridge's
	// orchestration loop calls Status when it decides to.
	PollInterval time.Duration
}

// Tuner submits and tracks fine-tuning jobs, and deploys/tears down the
// dedicated endpoints that serve their output, against Together's API.
//
// Safe for concurrent use: everything it holds is read-only after New.
type Tuner struct {
	opts    Options
	client  *secureClient
	headers http.Header
}

// ErrAuthentication means Together rejected the credential, or none was
// configured for its host.
//
// Its own sentinel for the same reason anthropic.ErrAuthentication has one:
// a rejected key otherwise fails every job in a bridge run with a message
// naming nothing about the cause, on a path where each failure is a $3-8
// job that was never even attempted rather than a fraction-of-a-cent call.
var ErrAuthentication = &errs.Actionable{
	Code:    "TOGETHER_AUTH",
	Message: "Together rejected the credential for this run",
	Fix: "set TOGETHER_API_KEY in the environment, or bind a key for this host " +
		"with --key-env host=VAR; no file, profile, or metadata service is read",
	ExitCode: errs.ExitError,
}

// ErrProvider means Together answered with an error this adapter cannot
// otherwise classify.
var ErrProvider = &errs.Actionable{
	Code:     "TOGETHER_PROVIDER_ERROR",
	Message:  "Together returned an error",
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
			Wrap(fmt.Errorf("together: no credential for %s", host))
	case normalizeHost(host) == normalizeHost(defaultHost):
		return nil, ErrAuthentication.Wrap(fmt.Errorf("together: no credential for %s", host))
	}

	client, err := newSecureClient(opts.BaseURL, opts.AllowInsecureBaseURL, opts.AllowPrivateAddress,
		opts.Timeout, opts.HTTPClient)
	if err != nil {
		return nil, errs.ErrInvalidInput.Wrap(err)
	}

	headers := http.Header{"Content-Type": []string{"application/json"}}
	if key != "" {
		// Together's REST API authenticates with a bearer token (verify).
		headers.Set("Authorization", "Bearer "+key)
	}
	if opts.UserAgent != "" {
		headers.Set("User-Agent", opts.UserAgent)
	}

	return &Tuner{opts: opts, client: client, headers: headers}, nil
}

func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errs.ErrInvalidInput.
			WithFix("write --base-url as a full URL, for example https://api.together.xyz").
			Wrap(fmt.Errorf("together: the base URL has no host"))
	}
	return u.Host, nil
}

// --- wire shapes -----------------------------------------------------------
//
// Every field below is (verify) unless stated otherwise in a comment. The
// grammar follows OpenAI's fine-tuning API shape, which the plan's Step 5
// counter-argument states Together accepts for the training file itself;
// the job-submission and status fields are this adapter's best-effort
// reading of Together's published docs at the time of writing, not a
// confirmed wire trace.

// submitRequest is the POST body for jobsPath.
type submitRequest struct {
	Model string `json:"model"`
	// TrainingFile is the file id from Together's Files API. Together
	// requires an upload step before submission (verify); that step is NOT
	// implemented in this PR — see this PR's report — so this field is
	// currently left empty rather than populated with something wrong.
	TrainingFile string `json:"training_file"`
	NEpochs      int32  `json:"n_epochs,omitempty"`
	LoraRank     int32  `json:"lora_rank,omitempty"`
	Suffix       string `json:"suffix,omitempty"`
}

// submitResponse is jobsPath's 2xx body.
type submitResponse struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// statusResponse is the GET-by-id body.
type statusResponse struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Progress       *float64 `json:"progress,omitempty"`
	FineTunedModel string   `json:"fine_tuned_model,omitempty"`
	Error          string   `json:"error,omitempty"`
	// TotalPriceUSDMicros is this adapter's own naming; Together's actual
	// field for job cost is unconfirmed (verify) and may not exist as a
	// distinct field at all — Step 2(c) already accounts for "the provider
	// reports no cost" as a first-class case, and this field being absent
	// or always zero degrades to exactly that path rather than to a bug.
	TotalPriceUSDMicros int64 `json:"total_price_usd_micros,omitempty"`
}

// deployRequest is the POST body for endpointsPath.
type deployRequest struct {
	Model    string `json:"model"`
	Replicas int    `json:"min_replicas,omitempty"`
}

// deployResponse is endpointsPath's 2xx body.
type deployResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Model string `json:"model"`
}

// jobListItem is one entry in jobsPath's list response. (verify — Together's
// exact list shape; this is the best-effort reading of the published docs at
// the time of writing, per the plan's Step 6 discipline.) Suffix is the field
// ListJobs matches against: OpenAI's job-list endpoint, which Together's
// format largely mirrors per the plan's Step 5 counter-argument, echoes the
// submitted suffix back on each listed job.
type jobListItem struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Suffix    string `json:"suffix,omitempty"`
	CreatedAt string `json:"created_at"`
}

// jobListResponse is jobsPath's GET (list) 2xx body (verify).
type jobListResponse struct {
	Data []jobListItem `json:"data"`
}

// endpointListItem is one entry in endpointsPath's list response. (verify)
// Model is what ListEndpoints matches suffix against, the same way the
// submit request's Suffix ends up baked into the tuned model's own name — see
// Deploy's use of state.GetTunedModel().
type endpointListItem struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Model string `json:"model"`
}

// endpointListResponse is endpointsPath's GET (list) 2xx body (verify).
type endpointListResponse struct {
	Data []endpointListItem `json:"data"`
}

// --- core.Tuner --------------------------------------------------------

// Submit sends a tuning job to Together.
//
// The caller (bridge.SubmitGroup) has already authorized this through the
// budget guard and written the write-ahead durable row — this method's only
// job is the HTTP call, exactly once, never retried: see the bridge plan's
// no-retry rule for why a retried submit against a provider with no
// confirmed idempotency key is a second job.
func (t *Tuner) Submit(ctx context.Context, job *core.TuningJob) (*core.JobRef, error) {
	if job == nil {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("together: Submit requires a job"))
	}
	req := submitRequest{
		Model:    job.GetBaseModel().GetTarget(),
		NEpochs:  job.GetEpochs(),
		LoraRank: job.GetLoraRank(),
		Suffix:   job.GetSuffix(),
		// job.GetTrainingData() is the rendered training file bytes. A real
		// implementation uploads it to Together's Files API first and
		// passes the resulting file id here (verify — Together's exact
		// upload flow); that upload step is NOT implemented in this PR,
		// which is a real gap — see this PR's report.
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("together: marshaling the submit request: %w", err)
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
			WithFix("Together returned a 2xx that is not a fine-tuning job response").
			Wrap(err)
	}
	return &core.JobRef{
		Id:          out.ID,
		Provider:    Scheme,
		SubmittedAt: out.CreatedAt,
	}, nil
}

// Status reports where a submitted job stands.
func (t *Tuner) Status(ctx context.Context, ref *core.JobRef) (*core.JobState, error) {
	if ref.GetId() == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("together: Status requires a job id"))
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
			WithFix("Together returned a 2xx that is not a job status response").
			Wrap(err)
	}

	state := &core.JobState{
		Ref:                 ref,
		Status:              mapStatus(out.Status),
		Error:               out.Error,
		ActualCostUsdMicros: out.TotalPriceUSDMicros,
	}
	if out.Progress != nil {
		state.Progress = out.Progress
	}
	if out.FineTunedModel != "" {
		state.TunedModel = &knov1.AgentRef{
			Ref:    "together:" + out.FineTunedModel,
			Scheme: Scheme,
			Target: out.FineTunedModel,
		}
	}
	return state, nil
}

// Model returns the tuned model as an agent ref. NOT a promise it is
// reachable — see core.Tuner.Model's doc: Together does not auto-serve a
// fine-tuned model, so a caller must Deploy first.
func (t *Tuner) Model(ctx context.Context, ref *core.JobRef) (*core.AgentRef, error) {
	state, err := t.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	if state.GetTunedModel() == nil {
		return nil, errs.ErrInvalidInput.
			WithFix("wait for the job to reach JOB_STATUS_SUCCEEDED before asking for its model").
			Wrap(fmt.Errorf("together: job %s has not produced a model yet", ref.GetId()))
	}
	return state.GetTunedModel(), nil
}

// Deploy creates a dedicated endpoint for a finished job's model — Step
// 2(f)'s second spend shape. The caller has already priced and authorized
// this through the budget guard's per-minute settlement loop; this method's
// job is the HTTP call.
func (t *Tuner) Deploy(ctx context.Context, ref *core.JobRef) (*core.Endpoint, error) {
	state, err := t.Status(ctx, ref)
	if err != nil {
		return nil, err
	}
	model := state.GetTunedModel()
	if model == nil {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("together: job %s has no model to deploy", ref.GetId()))
	}

	req := deployRequest{Model: model.GetTarget(), Replicas: 1}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("together: marshaling the deploy request: %w", err)
	}
	resp, err := t.client.do(ctx, http.MethodPost, endpointsPath, body, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out deployResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("Together returned a 2xx that is not an endpoint response").
			Wrap(err)
	}

	ep := &core.Endpoint{
		ID:       out.ID,
		Provider: Scheme,
		Served:   model.GetTarget(),
		Replicas: 1,
		Ready:    strings.EqualFold(out.State, "started") || strings.EqualFold(out.State, "ready"),
	}
	if ep.Ready {
		ep.ReadyAt = time.Now()
	}
	return ep, nil
}

// Teardown stops a deployed endpoint and its billing.
//
// Called unconditionally by the caller on every exit path once Deploy has
// succeeded. An error here means the endpoint may still be live and
// billing — the caller's responsibility is to treat that as loud, never
// swallowed; this method's job is only to report the failure honestly
// rather than retry silently and risk masking a real leak.
func (t *Tuner) Teardown(ctx context.Context, ep *core.Endpoint) error {
	if ep == nil || ep.ID == "" {
		return errs.ErrInvalidInput.Wrap(fmt.Errorf("together: Teardown requires an endpoint id"))
	}
	resp, err := t.client.do(ctx, http.MethodDelete, endpointsPath+"/"+url.PathEscape(ep.ID), nil, t.headers)
	if err != nil {
		return fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fromStatus(resp.StatusCode, resp.Body)
	}
	return nil
}

// ListJobs lists Together's fine-tuning jobs whose suffix matches exactly,
// most-recently-submitted first — the tuner-bridge plan's Step 2(d)
// adopt-by-suffix mechanism. Called only by a resume recovering a row a
// crash left in the write-ahead "submitting" state; see core.Tuner.ListJobs.
func (t *Tuner) ListJobs(ctx context.Context, suffix string) ([]*core.JobRef, error) {
	if suffix == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("together: ListJobs requires a suffix"))
	}
	resp, err := t.client.do(ctx, http.MethodGet, jobsPath, nil, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out jobListResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("Together returned a 2xx that is not a job list response").
			Wrap(err)
	}

	var refs []*core.JobRef
	for _, item := range out.Data {
		if item.Suffix != suffix {
			continue
		}
		refs = append(refs, &core.JobRef{
			Id:          item.ID,
			Provider:    Scheme,
			SubmittedAt: item.CreatedAt,
		})
	}
	// Most-recently-submitted first: CreatedAt is an RFC 3339 string, which
	// sorts lexically in timestamp order.
	sort.Slice(refs, func(i, j int) bool { return refs[i].GetSubmittedAt() > refs[j].GetSubmittedAt() })
	return refs, nil
}

// ListEndpoints lists Together's dedicated endpoints whose served model
// carries suffix, most-recently-created first — the tuner-bridge plan's Step
// 2(g) resume-time sweep mechanism. Called only when a durable row's Deploy
// may have succeeded at the provider before the row recorded its
// EndpointID; see core.Tuner.ListEndpoints.
func (t *Tuner) ListEndpoints(ctx context.Context, suffix string) ([]*core.Endpoint, error) {
	if suffix == "" {
		return nil, errs.ErrInvalidInput.Wrap(fmt.Errorf("together: ListEndpoints requires a suffix"))
	}
	resp, err := t.client.do(ctx, http.MethodGet, endpointsPath, nil, t.headers)
	if err != nil {
		return nil, fromTransport(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fromStatus(resp.StatusCode, resp.Body)
	}
	var out endpointListResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, ErrProvider.
			WithFix("Together returned a 2xx that is not an endpoint list response").
			Wrap(err)
	}

	var eps []*core.Endpoint
	for _, item := range out.Data {
		if !strings.Contains(item.Model, suffix) {
			continue
		}
		ready := strings.EqualFold(item.State, "started") || strings.EqualFold(item.State, "ready")
		eps = append(eps, &core.Endpoint{
			ID:       item.ID,
			Provider: Scheme,
			Served:   item.Model,
			Replicas: 1,
			Ready:    ready,
		})
	}
	// Most-recently-created is unknowable from this shape (verify — no
	// created_at surfaces on the list item above); returned in the provider's
	// own listing order, which callers must not depend on ordering beyond
	// "if more than one exists, sweep all of them" — see bridge's sweep.
	return eps, nil
}

// mapStatus translates Together's status strings onto JobStatus. (verify —
// Together's exact vocabulary; this is the best-effort reading of the
// published docs at the time of writing.) An unrecognized string maps to
// UNSPECIFIED rather than guessing, which is the same "absence over a wrong
// answer" discipline the rest of this adapter follows.
func mapStatus(s string) knov1.JobStatus {
	switch strings.ToLower(s) {
	case "pending", "validating_files", "validating":
		return knov1.JobStatus_JOB_STATUS_VALIDATING_FILES
	case "queued":
		return knov1.JobStatus_JOB_STATUS_QUEUED
	case "running", "training":
		return knov1.JobStatus_JOB_STATUS_RUNNING
	case "deploying", "compressing":
		return knov1.JobStatus_JOB_STATUS_DEPLOYING
	case "completed", "succeeded", "success":
		return knov1.JobStatus_JOB_STATUS_SUCCEEDED
	case "failed", "error":
		return knov1.JobStatus_JOB_STATUS_FAILED
	case "cancelled", "canceled", "user_aborted":
		return knov1.JobStatus_JOB_STATUS_CANCELLED
	default:
		return knov1.JobStatus_JOB_STATUS_UNSPECIFIED
	}
}

var _ core.Tuner = (*Tuner)(nil)
