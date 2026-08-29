package langsmith

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// DefaultEndpoint is the hosted LangSmith API root.
	DefaultEndpoint = "https://api.smith.langchain.com"

	// EndpointEnv overrides DefaultEndpoint, for self-hosted deployments.
	EndpointEnv = "LANGSMITH_ENDPOINT"

	// DefaultKeyEnv holds the credential every request carries, in the
	// x-api-key header LangSmith's API documents.
	DefaultKeyEnv = "LANGSMITH_API_KEY"
)

// Sizes and pacing. The row cap mirrors jsonl's maxLineBytes — a Case is a
// prompt and an expectation, not a corpus.
const (
	// maxRowBytes caps one example row, before mapping.
	maxRowBytes = 4 << 20 // 4 MiB

	// maxPageBytes caps one page envelope.
	maxPageBytes = maxRowBytes * 100

	// pageSize is the page size every request asks for; LangSmith documents
	// 100 as the maximum.
	pageSize = 100

	// requestTimeout bounds a single HTTP request. A page is small by design,
	// so a page that takes longer than this is broken, not slow.
	requestTimeout = 30 * time.Second

	// maxAttempts is how many times a 429 is retried, backing off with the
	// server's Retry-After.
	maxAttempts = 3

	// maxPages is the pagination backstop. A server that mints a fresh cursor
	// per page would otherwise spin forever; a REPEATED cursor is caught
	// immediately by the seen-cursor check, and this catches the fresh-cursor
	// variant.
	maxPages = 100_000
)

// Options configures a LangSmith Evals source.
type Options struct {
	// Dataset is the dataset name to read, e.g. "support-llm".
	//
	// LangSmith datasets are named, versioned, human-curated example rows.
	// The name is resolved to a dataset id at each iteration, so an edit to
	// the dataset shows up in the resume fingerprint rather than being
	// silently absorbed.
	Dataset string

	// Endpoint is the API root. Empty reads LANGSMITH_ENDPOINT, then
	// DefaultEndpoint.
	//
	// A plain-http or private-address endpoint is refused unless the Allow*
	// fields below opt in, because the API key would travel there.
	Endpoint string

	// APIKey is the credential. Empty reads LANGSMITH_API_KEY.
	//
	// Environment-only by design: there is deliberately no field that would
	// put a key value on a command line, where it lands in shell history, ps
	// output, and CI logs.
	APIKey string

	// HoldoutFrac is the share of Cases held back. Zero means
	// split.DefaultHoldoutFrac, exactly as the jsonl adapter maps it — the
	// dev-set size must not vary with the source.
	HoldoutFrac float64

	// SplitSeed deliberately re-splits an eval set. Normally empty; recorded
	// on the Run and part of the resume fingerprint.
	SplitSeed string

	// AllowInsecureBaseURL permits a plain-HTTP endpoint. The key rides the
	// connection in the clear otherwise, so the refusal is the default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 endpoints, for a
	// self-hosted LangSmith deployment on the local network. Link-local is
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

// Evals reads Cases from a LangSmith dataset.
//
// It satisfies core.Evals. Callers that must not see the holdout wrap it with
// core.Seal, which is a distinct type — so a stage that forgets does not
// compile.
type Evals struct {
	opts      Options
	endpoint  string // base URL, validated and trimmed of a trailing slash
	key       string // the credential; kept out of every error message
	transport *http.Client
}

// New returns an Evals reading from the named dataset.
//
// The dataset is not fetched here. Each call to Cases resolves the name and
// streams the rows, so every iteration gets an independent cursor — which is
// what lets the conformance harness run several iterations over one source
// and what lets a resumed run re-read from the start.
//
// The API key is required. A dataset without a key is a request that can
// never be satisfied, and discovering that mid-run, after the estimate has
// been printed, is a worse failure than refusing here.
func New(opts Options) (*Evals, error) {
	if opts.Dataset == "" {
		return nil, fmt.Errorf("langsmith: no dataset name given")
	}
	if opts.HoldoutFrac < 0 || opts.HoldoutFrac >= 1 {
		return nil, fmt.Errorf("langsmith: holdout fraction %v must be in [0, 1)", opts.HoldoutFrac)
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = os.Getenv(EndpointEnv)
	}
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if err := checkEndpoint(endpoint, opts.AllowInsecureBaseURL, opts.AllowPrivateAddress); err != nil {
		return nil, err
	}

	key := opts.APIKey
	if key == "" {
		key = os.Getenv(DefaultKeyEnv)
	}
	if key == "" {
		return nil, fmt.Errorf("langsmith: no %s set; the dataset API authenticates every request with it", DefaultKeyEnv)
	}

	return &Evals{
		opts:      opts,
		endpoint:  strings.TrimRight(endpoint, "/"),
		key:       key,
		transport: newHTTPClient(opts.HTTPClient, opts.AllowPrivateAddress),
	}, nil
}

// dataset is the resolved dataset metadata that names the source.
type dataset struct {
	ID           string
	Name         string
	ModifiedAt   string // raw API value; part of the resume fingerprint
	ExampleCount int64
}

// lookupDataset resolves the configured dataset name to a dataset id.
//
// Datasets are matched by exact name, across every page of the listing —
// a name beyond the first page is still found, and the multi-match guard
// sees later pages too. Zero matches is a fatal error naming the dataset,
// and more than one is equally fatal — a name that resolves to two datasets
// would make the resume fingerprint depend on which one the server returned
// first. A row with any other name is dropped rather than counted: the name
// query was ours, so a row with another name is a server that ignored it,
// and counting such rows would put the fingerprint at the server's mercy.
func (e *Evals) lookupDataset(ctx context.Context) (*dataset, error) {
	var matches []*dataset
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := pageLimitError(page); err != nil {
			return nil, err
		}
		if _, dup := seenCursors[cursor]; dup {
			return nil, fmt.Errorf("langsmith: the datasets API served cursor %q again; a "+
				"repeating cursor means the pagination is not advancing", cursor)
		}
		seenCursors[cursor] = struct{}{}

		q := url.Values{}
		q.Set("name", e.opts.Dataset)
		q.Set("limit", strconv.Itoa(pageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := e.do(ctx, "/datasets", q)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("langsmith: the datasets API answered %s", e.redact(resp.Status))
		}

		// The envelope is the same {items, next_cursor} shape the examples
		// endpoint uses; the fields this source needs are id, name,
		// modified_at, and example_count, and anything else in a dataset
		// object is ignored.
		pr, err := newPageReader(http.MaxBytesReader(nil, resp.Body, maxPageBytes))
		if err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("langsmith: decoding the datasets response: %w", err)
		}
		for {
			raw, ok, err := pr.nextRow()
			if err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("langsmith: decoding the datasets response: %w", err)
			}
			if !ok {
				break
			}
			kvs, err := orderedObject(raw)
			if err != nil {
				_ = resp.Body.Close()
				return nil, fmt.Errorf("langsmith: decoding the datasets response: %w", err)
			}
			var ds dataset
			for _, kv := range kvs {
				switch kv.key {
				case "id":
					_ = json.Unmarshal(kv.val, &ds.ID)
				case "name":
					_ = json.Unmarshal(kv.val, &ds.Name)
				case "modified_at":
					_ = json.Unmarshal(kv.val, &ds.ModifiedAt)
				case "example_count":
					_ = json.Unmarshal(kv.val, &ds.ExampleCount)
				}
			}
			// The name query was ours, so a row with any other name is a
			// server that ignored it — counting such rows would make the
			// fingerprint depend on what the server decided to return.
			if ds.Name == e.opts.Dataset {
				matches = append(matches, &ds)
			}
		}
		_ = resp.Body.Close()

		next, ok := pr.nextCursor()
		if !ok {
			break
		}
		cursor = next
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("langsmith: no dataset named %q at %s", e.opts.Dataset, e.endpoint)
	case 1:
		if matches[0].ID == "" {
			return nil, fmt.Errorf("langsmith: dataset %q came back without an id", e.opts.Dataset)
		}
		return matches[0], nil
	default:
		return nil, fmt.Errorf("langsmith: %d datasets match the name %q; the fingerprint "+
			"must not depend on which one the server returns first", len(matches), e.opts.Dataset)
	}
}

// Cases yields every example in the dataset, each mapped to a Case with its
// split assigned.
//
// Contract, per core.Evals:
//
//   - A yielded error is FATAL. A malformed row, a duplicate example id, or
//     an oversized row stops the run — silently dropping records would
//     shrink the denominator behind every later delta without anything
//     showing it, and LangSmith itself would hand the record back on a
//     re-run, changing the population under an identical resume.
//   - Cleanup is deferred INSIDE the closure, so an early break still closes
//     the open page.
//   - ctx is checked before each yield.
//   - Yielded Cases are borrowed for one iteration. The backing record is
//     reused; clone before retaining.
//
// The dataset name resolves to a dataset id on every call, so a renamed or
// deleted dataset is a hard error here — reported from Cases itself, with
// nothing yielded — rather than a silently different eval set.
func (e *Evals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	ds, err := e.lookupDataset(ctx)
	if err != nil {
		return nil, err
	}

	frac := e.opts.holdoutFrac()
	seed := e.opts.SplitSeed

	return func(yield func(*core.Case, error) bool) {
		// seen is scoped per iteration: a duplicate example id is fatal
		// WITHIN one iteration, and a re-read of the dataset may see the
		// same ids again.
		seen := make(map[string]struct{})

		// Cursors seen so far, so a repeating cursor fails loudly instead of
		// spinning on a server that answers every page with the first one.
		cursors := make(map[string]struct{})

		cursor := ""
		for page := 1; ; page++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if err := pageLimitError(page); err != nil {
				yield(nil, err)
				return
			}
			if _, dup := cursors[cursor]; dup {
				yield(nil, fmt.Errorf("langsmith: dataset %q served cursor %q again; a "+
					"repeating cursor means the pagination is not advancing, so the stream "+
					"stops here rather than spin", ds.Name, cursor))
				return
			}
			cursors[cursor] = struct{}{}

			body, err := e.openPage(ctx, ds.ID, cursor, ds.Name)
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
			// happened. Close is idempotent, so the explicit close below
			// only releases the connection early.
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

				ex, err := decodeRow(raw)
				if err != nil {
					yield(nil, err)
					return
				}
				if ex.ID == "" {
					// Fatal, not defaulted — the dev/holdout split is keyed on
					// the id, so a row without one could not stay stable
					// across re-reads. Same reasoning as jsonl's missing-id
					// fatal.
					yield(nil, fmt.Errorf("langsmith: example has no id, and the dev/holdout "+
						"split is keyed on it; give every example a stable id"))
					return
				}
				if ex.Input == "" {
					yield(nil, fmt.Errorf("langsmith: example %q has no input", ex.ID))
					return
				}
				if _, dup := seen[ex.ID]; dup {
					// Duplicate ids are fatal rather than tolerated: the split
					// is keyed on the id, so two rows sharing one would land
					// in the same half and be indistinguishable in every later
					// report. Same invariant jsonl enforces (docs/debt.md#45).
					yield(nil, fmt.Errorf("langsmith: duplicate example id %q", ex.ID))
					return
				}
				seen[ex.ID] = struct{}{}

				c := &core.Case{
					Id:       ex.ID,
					Input:    ex.Input,
					Expected: ex.Expected,
					Split:    split.AssignSplit(ex.ID, seed, frac),
					Provenance: &knov1.Provenance{
						Source:         "langsmith",
						SourceRef:      "dataset:" + ds.Name + ":" + ex.ID,
						Derived:        true,
						DerivationNote: ex.DerivationNote,
					},
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

// openPage fetches one page of examples and returns its body, capped so a
// page larger than the envelope cap is cut off loudly rather than buffered.
//
// datasetName is echoed into mid-pagination errors, so a run over several
// datasets can tell which one refused mid-stream.
func (e *Evals) openPage(ctx context.Context, datasetID, cursor, datasetName string) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("dataset_id", datasetID)
	q.Set("limit", strconv.Itoa(pageSize))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	resp, err := e.do(ctx, "/examples", q)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("langsmith: dataset %q: the examples API answered %s",
			datasetName, e.redact(resp.Status))
	}
	return http.MaxBytesReader(nil, resp.Body, maxPageBytes), nil
}

// CountSplits reads the dataset and reports how it divides, without yielding
// Cases.
//
// Ingestion needs the counts up front: a zero-Case holdout must be refused
// before any money is spent, not discovered at Validate after a full run.
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
// It is deliberately NOT a content hash: hashing the examples would require
// a full second pass over a remote dataset, at remote latency, for a
// fingerprint that changes anyway when the dataset does. The dataset id,
// modified_at, and example count are the metadata the one lookupDataset pass
// already returns, and they catch every edit path that matters — a renamed
// dataset, an added or removed example, a bulk edit. An in-place edit that
// leaves all three untouched goes undetected; that is the accepted tradeoff,
// recorded in docs/plans/2026-08-28-langsmith-evals-adapter.md.
//
// The split configuration IS in the fingerprint, folded in exactly as the
// jsonl adapter folds it: a different SplitSeed or HoldoutFrac re-divides
// the same eval set, so the fingerprint must move with them, or a resumed
// run would restore the old division's checkpoint under the new division's
// plan. The name is folded in too — a dataset recreated under the same
// metadata, or a renamed dataset that id-based content cannot see, still
// moves the fingerprint.
func (e *Evals) ContentHash(ctx context.Context) (string, error) {
	ds, err := e.lookupDataset(ctx)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = io.WriteString(h, "langsmith:"+ds.ID+":"+ds.ModifiedAt+":"+strconv.FormatInt(ds.ExampleCount, 10))
	_, _ = h.Write(split.FingerprintSplit(e.opts.SplitSeed, e.opts.holdoutFrac()))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(ds.Name))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Evals = (*Evals)(nil)
