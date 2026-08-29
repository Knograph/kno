package braintrust

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// Environment and defaults. The key is environment-only by design: a key on
// a command line lands in shell history, in ps output, and in CI logs, so
// the CLI deliberately has no flag for it.
const (
	// DefaultHost is the hosted Braintrust API root, the same base URL the
	// vendor's own SDKs default to.
	DefaultHost = "https://api.braintrust.dev"

	// HostEnv overrides DefaultHost, for self-hosted deployments. The name
	// matches the vendor's own SDKs, so a deployment that needs an override
	// sets one variable for every tool that reads the dataset.
	HostEnv = "BRAINTRUST_API_BASE_URL"

	// KeyEnv holds the credential every request carries, as a Bearer
	// token per Braintrust's API documentation.
	KeyEnv = "BRAINTRUST_API_KEY"

	// OrgNameEnv optionally selects the org the requests run in. It matters
	// only for keys that span orgs, and it travels as a query parameter on
	// both endpoints, because Braintrust's API reads it from the query.
	OrgNameEnv = "BRAINTRUST_ORG_NAME"
)

// Sizes and pacing. The row cap mirrors jsonl's maxLineBytes — a Case is a
// prompt and an expectation, not a corpus.
const (
	// maxRowBytes caps one dataset event row, before mapping.
	maxRowBytes = 4 << 20 // 4 MiB

	// maxPageBytes caps one page envelope.
	maxPageBytes = maxRowBytes * 100

	// pageSize is the page size every request asks for. Braintrust's fetch
	// limit counts TRACES rather than rows, so a page can legally exceed
	// pageSize events — the row and page byte caps are the real bounds.
	pageSize = 100

	// requestTimeout bounds a single HTTP request. A page is small by design,
	// so a page that takes longer than this is broken, not slow.
	requestTimeout = 30 * time.Second

	// maxAttempts is how many times a 429 is retried, backing off with the
	// server's Retry-After.
	maxAttempts = 3

	// maxPages is the pagination backstop. A server that mints a fresh
	// cursor per page would otherwise spin forever; a REPEATED cursor is
	// caught immediately by the seen-cursor check, and this catches the
	// fresh-cursor variant.
	maxPages = 100_000
)

// Options configures a Braintrust Evals source.
type Options struct {
	// Dataset is the dataset name to read, e.g. "support-llm".
	//
	// The name is resolved at each iteration, so an edit to the dataset shows
	// up in the resume fingerprint rather than being silently absorbed.
	Dataset string

	// Host is the API root. Empty reads BRAINTRUST_API_BASE_URL, then
	// DefaultHost.
	//
	// A plain-http or private-address host is refused unless the Allow*
	// fields below opt in, because the API key travels there as a Bearer
	// token on every request.
	Host string

	// APIKey is the credential. Empty reads BRAINTRUST_API_KEY.
	//
	// Environment-only by design: there is deliberately no field that would
	// put a key value on a command line, where it lands in shell history, ps
	// output, and CI logs.
	APIKey string

	// OrgName optionally selects the org the requests run in. Empty reads
	// BRAINTRUST_ORG_NAME; both default to "". It matters only for keys
	// that span orgs, and it is sent as a query parameter.
	OrgName string

	// HoldoutFrac is the share of Cases held back. Zero means
	// split.DefaultHoldoutFrac, exactly as the jsonl adapter maps it — the
	// dev-set size must not vary with the source.
	HoldoutFrac float64

	// SplitSeed deliberately re-splits an eval set. Normally empty; recorded
	// on the Run and part of the resume fingerprint.
	SplitSeed string

	// AllowInsecureBaseURL permits a plain-HTTP host. The key rides the
	// connection in the clear otherwise, so the refusal is the default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 endpoints, for a
	// self-hosted Braintrust deployment on the local network. Link-local is
	// never permitted — 169.254.169.254 is the instance-metadata endpoint.
	AllowPrivateAddress bool

	// HTTPClient supplies transport and TLS settings only; its redirect
	// policy and timeout are always overwritten.
	HTTPClient *http.Client
}

func (o Options) holdoutFrac() float64 {
	if o.HoldoutFrac <= 0 {
		return split.DefaultHoldoutFrac
	}
	return o.HoldoutFrac
}

// Evals reads Cases from a Braintrust dataset.
//
// It satisfies core.Evals. Callers that must not see the holdout wrap it with
// core.Seal, which is a distinct type — so a stage that forgets does not
// compile.
type Evals struct {
	opts      Options
	endpoint  string // base URL, validated and trimmed of a trailing slash
	key       string // the credential; kept out of every error message
	org       string // optional org_name query parameter
	bearer    string // the Authorization header value, redacted from errors
	transport *http.Client
}

// New returns an Evals reading from the named dataset.
//
// The dataset is not fetched here. Each call to Cases resolves the name and
// streams the events, so every iteration gets an independent cursor — which
// is what lets the conformance harness run several iterations over one
// source and what lets a resumed run re-read from the start.
//
// The API key is required. A dataset without a key is a request that can
// never be satisfied, and discovering that mid-run, after the estimate has
// been printed, is a worse failure than refusing here.
func New(opts Options) (*Evals, error) {
	if opts.Dataset == "" {
		return nil, fmt.Errorf("braintrust: no dataset name given")
	}
	if opts.HoldoutFrac < 0 || opts.HoldoutFrac >= 1 {
		return nil, fmt.Errorf("braintrust: holdout fraction %v must be in [0, 1)", opts.HoldoutFrac)
	}

	endpoint := opts.Host
	if endpoint == "" {
		endpoint = os.Getenv(HostEnv)
	}
	if endpoint == "" {
		endpoint = DefaultHost
	}
	if err := checkEndpoint(endpoint, opts.AllowInsecureBaseURL, opts.AllowPrivateAddress); err != nil {
		return nil, err
	}

	key := opts.APIKey
	if key == "" {
		key = os.Getenv(KeyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("braintrust: no %s set; the dataset API authenticates "+
			"every request with it", KeyEnv)
	}

	org := opts.OrgName
	if org == "" {
		org = os.Getenv(OrgNameEnv)
	}

	e := &Evals{
		opts:     opts,
		endpoint: strings.TrimRight(endpoint, "/"),
		key:      key,
		org:      org,
		bearer:   bearerValue(key),
	}
	e.transport = newHTTPClient(opts.HTTPClient, opts.AllowPrivateAddress)
	return e, nil
}

// dataset is the resolved dataset metadata that names the source.
type dataset struct {
	ID   string
	Name string
}

// resolveDataset resolves the configured dataset name via the /v1/dataset
// list endpoint, filtered by dataset_name.
//
// The endpoint is a FILTER, not a lookup: an unknown name is answered 200
// with an empty array, not a 404. So a miss must be detected here, in the
// resolution pass, and refused loudly naming the dataset and the host — the
// fetch endpoint would hand back an empty page for both a miss and a
// dataset that exists and is empty, and "exists and is empty" is a
// different claim from "no such dataset".
func (e *Evals) resolveDataset(ctx context.Context) (*dataset, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	q := url.Values{}
	q.Set("dataset_name", e.opts.Dataset)
	if e.org != "" {
		q.Set("org_name", e.org)
	}
	resp, err := e.do(ctx, "/v1/dataset", q)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("braintrust: the datasets API answered %s", e.redact(resp.Status))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("braintrust: reading the datasets response: %w", err)
	}
	if int64(len(body)) > maxPageBytes {
		return nil, fmt.Errorf("braintrust: the datasets response exceeded the %d-byte page cap", maxPageBytes)
	}

	all, err := decodeDatasetList(body)
	if err != nil {
		return nil, fmt.Errorf("braintrust: decoding the datasets response: %w", err)
	}

	// The name query was ours, so a dataset that answered with a different
	// name is a server that ignored the filter. A single wrong-named row is
	// refused loudly — the fingerprint must not depend on a server that
	// ignores the filter — and a row with any other name is dropped from
	// the match count, langsmith-style.
	if len(all) == 1 && all[0].Name != "" && all[0].Name != e.opts.Dataset {
		return nil, fmt.Errorf("braintrust: the datasets API answered for dataset %q when %q "+
			"was asked for; the fingerprint must not depend on a server that "+
			"ignores the filter", all[0].Name, e.opts.Dataset)
	}
	var matches []*dataset
	for _, ds := range all {
		if ds.Name == e.opts.Dataset {
			matches = append(matches, ds)
		}
	}

	switch len(matches) {
	case 0:
		msg := fmt.Sprintf("braintrust: no dataset named %q at %s; check the name and %s",
			e.opts.Dataset, e.endpoint, HostEnv)
		if e.org != "" {
			msg += "; " + OrgNameEnv + " is set, so check that the dataset lives in that org"
		}
		return nil, errors.New(msg)
	case 1:
		if matches[0].ID == "" {
			return nil, fmt.Errorf("braintrust: dataset %q came back without an id", e.opts.Dataset)
		}
		return matches[0], nil
	default:
		return nil, fmt.Errorf("braintrust: %d datasets match the name %q; the fingerprint "+
			"must not depend on which one the server returns first", len(matches), e.opts.Dataset)
	}
}

// Cases yields every event in the dataset, each mapped to a Case with its
// split assigned.
//
// Contract, per core.Evals:
//
//   - A yielded error is FATAL. A malformed row, an event with a null
//     input, or an oversized row stops the run — silently dropping records
//     would shrink the denominator behind every later delta without
//     anything showing it, and Braintrust itself would hand the record
//     back on a re-run, changing the population under an identical resume.
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     the open page.
//   - ctx is checked before each yield.
//   - Yielded Cases are borrowed for one iteration. The backing record is
//     reused; clone before retaining.
//
// The dataset name resolves on every call, so a renamed or deleted dataset
// is a hard error here — reported from Cases itself, with nothing yielded —
// rather than a silently different eval set.
//
// Duplicate ids are merged, not fatal: Braintrust's pagination walks the
// dataset's version history, and the vendor documents that a later page may
// return a row that already appeared, with an EARLIER _xact_id — the walk
// serves versions newest-first. So the first occurrence of an id IS its
// newest version, and a later duplicate is the same row's pre-edit version
// re-exposed; the merge keeps the newest and drops the duplicate. An edit
// mid-pagination surfaces as exactly these duplicates, and the merge rule
// is the response, not a refusal — a fatal would make every edited
// multi-page dataset unrunnable (plan P0-2, fixture-pinned).
func (e *Evals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	ds, err := e.resolveDataset(ctx)
	if err != nil {
		return nil, err
	}

	frac := e.opts.holdoutFrac()
	seed := e.opts.SplitSeed

	return func(yield func(*core.Case, error) bool) {
		// seen is scoped per iteration: a duplicate id is a merge WITHIN
		// one iteration, and a re-read of the dataset may see the same ids
		// again.
		seen := make(map[string]struct{})

		cursor := ""
		seenCursors := make(map[string]struct{})
		for page := 1; ; page++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if err := pageLimitError(page); err != nil {
				yield(nil, err)
				return
			}
			if _, dup := seenCursors[cursor]; dup {
				yield(nil, fmt.Errorf("braintrust: dataset %q: the fetch API served cursor %q "+
					"again; a repeating cursor means the pagination is not advancing", ds.Name, cursor))
				return
			}
			seenCursors[cursor] = struct{}{}

			body, err := e.openPage(ctx, ds.ID, ds.Name, cursor)
			if err != nil {
				yield(nil, err)
				return
			}
			pr, err := newPageReader(body)
			if err != nil {
				_ = body.Close()
				yield(nil, err)
				return
			}
			// Inside the closure: an early break must still close the open
			// page, and a deferred close registered outside this function
			// would not run until Cases' own return, which has already
			// happened. Close is idempotent, so the explicit close below only
			// releases the connection early.
			defer func() { _ = body.Close() }()

			for {
				raw, ok, err := pr.nextRow()
				if err != nil {
					yield(nil, err)
					return
				}
				if !ok {
					break
				}
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}

				ev, err := decodeEvent(raw)
				if err != nil {
					yield(nil, err)
					return
				}
				if ev.ID == "" {
					// Fatal, not defaulted — the dev/holdout split is keyed on
					// the id, so a row without one could not stay stable
					// across re-reads. Same reasoning as jsonl's missing-id
					// fatal.
					yield(nil, fmt.Errorf("braintrust: event has no id, and the dev/holdout "+
						"split is keyed on it; give every event a stable id"))
					return
				}
				if ev.XactID == "" {
					// Fatal, not defaulted — the version counter is the merge
					// rule's signal and the resume fingerprint's freshness
					// half, so a row without one cannot be placed in the
					// version history at all.
					yield(nil, fmt.Errorf("braintrust: event %s has no _xact_id; the dedupe "+
						"rule and the resume fingerprint are keyed on it", displayID(ev.ID)))
					return
				}
				if _, dup := seen[ev.ID]; dup {
					// The merge rule (see the doc comment above): a later page
					// re-served a version of an id already yielded, and the
					// first occurrence was the newest version. Skipped, never
					// yielded and never fatal.
					continue
				}
				seen[ev.ID] = struct{}{}

				provenance := &knov1.Provenance{
					Source:         "braintrust",
					SourceRef:      "dataset:" + ds.Name + ":" + ev.ID,
					Derived:        ev.Derived,
					DerivationNote: ev.DerivationNote,
				}
				c := &core.Case{
					Id:         ev.ID,
					Input:      ev.Input,
					Expected:   ev.Expected,
					Split:      split.AssignSplit(ev.ID, seed, frac),
					Provenance: provenance,
				}
				if !yield(c, nil) {
					return
				}
			}

			// Release the page before opening the next one; the deferred
			// close still covers the early-break path.
			_ = body.Close()

			next, ok := pr.nextCursor()
			if !ok {
				return
			}
			cursor = next
		}
	}, nil
}

// openPage fetches one page of dataset events and returns its body, capped
// so a page larger than the envelope cap is cut off loudly rather than
// buffered.
//
// datasetName is echoed into mid-pagination errors, so a run over several
// datasets can tell which one refused mid-stream.
func (e *Evals) openPage(ctx context.Context, datasetID, datasetName, cursor string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(pageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if e.org != "" {
		q.Set("org_name", e.org)
	}
	resp, err := e.do(ctx, "/v1/dataset/"+url.PathEscape(datasetID)+"/fetch", q)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("braintrust: dataset %q: the fetch API answered %s",
			datasetName, e.redact(resp.Status))
	}
	return http.MaxBytesReader(nil, resp.Body, maxPageBytes), nil
}

// CountSplits reads the dataset and reports how it divides, without yielding
// Cases.
//
// Ingestion needs the counts up front: a zero-Case holdout must be refused
// before any money is spent, not discovered at Validate after a full run.
//
// Weak labels are counted here, jsonl-identical: a Case marked
// Provenance.Derived is an event copied from another object (origin set),
// and the Run records the count at close so a weak-label eval cannot pass
// for a hand-authored one.
func (e *Evals) CountSplits(ctx context.Context) (SplitCounts, error) {
	seq, err := e.Cases(ctx)
	if err != nil {
		return SplitCounts{}, err
	}

	counts := SplitCounts{HoldoutFrac: e.opts.holdoutFrac()}
	for c, err := range seq {
		if err != nil {
			return SplitCounts{}, err
		}
		if c.GetProvenance().GetDerived() {
			counts.WeakLabelCases++
		}
		if c.GetSplit() == knov1.Split_SPLIT_HOLDOUT {
			counts.Holdout++
			continue
		}
		counts.Dev++
	}
	return counts, nil
}

// ContentHash is the eval source's fingerprint for the Run's resume
// fingerprint.
//
// It is deliberately NOT a content hash: hashing the events would require a
// full pass over a remote dataset, at remote latency, for a fingerprint
// that changes anyway when the dataset does. Braintrust's dataset object
// carries NO updatedAt or revision field to lean on (verified in the plan),
// so the freshness half of the fingerprint is the version counter of the
// dataset's newest event, read with a fetch?limit=1 request. Host + dataset
// id + dataset name + newest _xact_id has the same edit sensitivity at two
// requests: a dataset renamed, recreated, or edited moves one of the four
// halves, and a resumed run re-resolves and re-fetches before it trusts
// any checkpoint. An edit that leaves the newest event's _xact_id untouched
// goes undetected; that is the accepted tradeoff, recorded in
// docs/plans/2026-08-29-braintrust-evals-adapter.md. When Braintrust
// exposes a dataset-level version (the plan's v0.2 deferral), that version
// joins this hash.
//
// The split configuration IS in the fingerprint, folded in exactly as the
// jsonl adapter folds it: a different SplitSeed or HoldoutFrac re-divides
// the same eval set, so the fingerprint must move with them, or a resumed
// run would restore the old division's checkpoint under the new division's
// plan.
func (e *Evals) ContentHash(ctx context.Context) (string, error) {
	ds, err := e.resolveDataset(ctx)
	if err != nil {
		return "", err
	}
	xid, err := e.firstEventXactID(ctx, ds.ID, ds.Name)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = io.WriteString(h, "braintrust:"+e.endpoint+":"+ds.ID+":"+ds.Name+":"+xid)
	_, _ = h.Write(split.FingerprintSplit(e.opts.SplitSeed, e.opts.holdoutFrac()))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// firstEventXactID reads the dataset's newest event's version counter with
// a fetch?limit=1 request. The fetch limit counts TRACES, not rows, so the
// response can legally exceed one event; the FIRST event of the array is
// the newest, which is the one the fingerprint is keyed on.
func (e *Evals) firstEventXactID(ctx context.Context, datasetID, datasetName string) (string, error) {
	q := url.Values{}
	q.Set("limit", "1")
	if e.org != "" {
		q.Set("org_name", e.org)
	}
	resp, err := e.do(ctx, "/v1/dataset/"+url.PathEscape(datasetID)+"/fetch", q)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return "", fmt.Errorf("braintrust: dataset %q: the fetch API answered %s",
			datasetName, e.redact(resp.Status))
	}
	pr, err := newPageReader(http.MaxBytesReader(nil, resp.Body, maxPageBytes))
	if err != nil {
		_ = resp.Body.Close()
		return "", fmt.Errorf("braintrust: decoding the fetch response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, ok, err := pr.nextRow()
	if err != nil {
		return "", fmt.Errorf("braintrust: decoding the fetch response: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("braintrust: dataset %q has no events, so it has no _xact_id "+
			"for the resume fingerprint; the eval set is empty", datasetName)
	}
	xid, err := extractXactID(raw)
	if err != nil {
		return "", fmt.Errorf("braintrust: dataset %q: %w", datasetName, err)
	}
	return xid, nil
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Evals = (*Evals)(nil)

// decodeDatasetList maps the /v1/dataset response — a bare JSON array of
// dataset objects — to the metadata this source needs.
func decodeDatasetList(raw []byte) ([]*dataset, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("the response is not a JSON array")
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("the response is not a JSON array")
	}
	var out []*dataset
	for dec.More() {
		var rawObj json.RawMessage
		if err := dec.Decode(&rawObj); err != nil {
			return nil, fmt.Errorf("a dataset object could not be decoded")
		}
		kvs, err := orderedObject(rawObj)
		if err != nil {
			return nil, fmt.Errorf("a dataset object is not a JSON object")
		}
		ds := &dataset{}
		for _, kv := range kvs {
			switch kv.key {
			case "id":
				if err := json.Unmarshal(kv.val, &ds.ID); err != nil {
					return nil, fmt.Errorf("the dataset id is not a string")
				}
			case "name":
				if err := json.Unmarshal(kv.val, &ds.Name); err != nil {
					return nil, fmt.Errorf("the dataset name is not a string")
				}
			}
		}
		out = append(out, ds)
	}
	return out, nil
}
