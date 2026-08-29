package langfuse

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

// Environment and defaults. The keys are environment-only by design: a key
// on a command line lands in shell history, in ps output, and in CI logs, so
// the CLI deliberately has no flag for them.
const (
	// DefaultHost is the hosted Langfuse API root.
	DefaultHost = "https://cloud.langfuse.com"

	// HostEnv overrides DefaultHost, for self-hosted deployments.
	HostEnv = "LANGFUSE_HOST"

	// PublicKeyEnv and SecretKeyEnv hold the credential every request
	// carries, as HTTP basic auth (public key as user, secret key as
	// password) per Langfuse's API documentation.
	PublicKeyEnv = "LANGFUSE_PUBLIC_KEY"
	SecretKeyEnv = "LANGFUSE_SECRET_KEY"
)

// Sizes and pacing. The row cap mirrors jsonl's maxLineBytes — a Case is a
// prompt and an expectation, not a corpus.
const (
	// maxRowBytes caps one dataset item row, before mapping.
	maxRowBytes = 4 << 20 // 4 MiB

	// maxPageBytes caps one page envelope.
	maxPageBytes = maxRowBytes * 100

	// pageSize is the page size every request asks for; Langfuse documents
	// 100 as the maximum.
	pageSize = 100

	// requestTimeout bounds a single HTTP request. A page is small by design,
	// so a page that takes longer than this is broken, not slow.
	requestTimeout = 30 * time.Second

	// maxAttempts is how many times a 429 is retried, backing off with the
	// server's Retry-After.
	maxAttempts = 3

	// maxPages is the pagination backstop. A server whose meta never advances
	// would otherwise spin forever.
	maxPages = 100_000
)

// Options configures a Langfuse Evals source.
type Options struct {
	// Dataset is the dataset name to read, e.g. "support-llm".
	//
	// The name is resolved at each iteration, so an edit to the dataset shows
	// up in the resume fingerprint rather than being silently absorbed.
	Dataset string

	// Host is the API root. Empty reads LANGFUSE_HOST, then DefaultHost.
	//
	// A plain-http or private-address host is refused unless the Allow*
	// fields below opt in, because the keys travel there as basic auth —
	// base64, not encryption.
	Host string

	// PublicKey is the basic-auth user. Empty reads LANGFUSE_PUBLIC_KEY.
	//
	// Environment-only by design: there is deliberately no field that would
	// put a key value on a command line, where it lands in shell history, ps
	// output, and CI logs.
	PublicKey string

	// SecretKey is the basic-auth password. Empty reads LANGFUSE_SECRET_KEY.
	//
	// Environment-only, for the same reason as PublicKey.
	SecretKey string

	// HoldoutFrac is the share of Cases held back. Zero means
	// split.DefaultHoldoutFrac, exactly as the jsonl adapter maps it — the
	// dev-set size must not vary with the source.
	HoldoutFrac float64

	// SplitSeed deliberately re-splits an eval set. Normally empty; recorded
	// on the Run and part of the resume fingerprint.
	SplitSeed string

	// AllowInsecureBaseURL permits a plain-HTTP host. The keys ride the
	// connection in the clear otherwise (basic auth is base64, not
	// encryption), so the refusal is the default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 endpoints, for a
	// self-hosted Langfuse deployment on the local network. Link-local is
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

// Evals reads Cases from a Langfuse dataset.
//
// It satisfies core.Evals. Callers that must not see the holdout wrap it with
// core.Seal, which is a distinct type — so a stage that forgets does not
// compile.
type Evals struct {
	opts      Options
	endpoint  string // base URL, validated and trimmed of a trailing slash
	publicKey string
	secretKey string
	// basic is the Authorization header value ("Basic <base64>"), redacted
	// from every error message alongside the secret key itself.
	basic     string
	transport *http.Client
}

// New returns an Evals reading from the named dataset.
//
// The dataset is not fetched here. Each call to Cases resolves the name and
// streams the items, so every iteration gets an independent cursor — which is
// what lets the conformance harness run several iterations over one source
// and what lets a resumed run re-read from the start.
//
// Both keys are required. A dataset without them is a request that can never
// be satisfied, and discovering that mid-run, after the estimate has been
// printed, is a worse failure than refusing here.
func New(opts Options) (*Evals, error) {
	if opts.Dataset == "" {
		return nil, fmt.Errorf("langfuse: no dataset name given")
	}
	if opts.HoldoutFrac < 0 || opts.HoldoutFrac >= 1 {
		return nil, fmt.Errorf("langfuse: holdout fraction %v must be in [0, 1)", opts.HoldoutFrac)
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

	publicKey := opts.PublicKey
	if publicKey == "" {
		publicKey = os.Getenv(PublicKeyEnv)
	}
	secretKey := opts.SecretKey
	if secretKey == "" {
		secretKey = os.Getenv(SecretKeyEnv)
	}
	if publicKey == "" || secretKey == "" {
		return nil, fmt.Errorf("langfuse: no %s and %s set; the dataset API "+
			"authenticates every request with both, as basic auth", PublicKeyEnv, SecretKeyEnv)
	}

	e := &Evals{
		opts:      opts,
		endpoint:  strings.TrimRight(endpoint, "/"),
		publicKey: publicKey,
		secretKey: secretKey,
		basic:     basicAuthValue(publicKey, secretKey),
	}
	e.transport = newHTTPClient(opts.HTTPClient, opts.AllowPrivateAddress)
	return e, nil
}

// dataset is the resolved dataset metadata that names the source.
type dataset struct {
	ID        string
	Name      string
	UpdatedAt string // raw API value; part of the resume fingerprint
}

// resolveDataset resolves the configured dataset name via the v2 datasets
// endpoint, which 404s for an unknown name.
//
// The items endpoint does NOT 404 for a typo'd dataset — datasetName is a
// filter there, and a miss returns 200 with an empty data array. So the name
// is resolved FIRST: a typo is refused loudly with an actionable error naming
// the dataset and the host, before any page is fetched, and an empty dataset
// that exists stays legal — "exists and is empty" is a different claim from
// "no such dataset", and only this pass can tell them apart.
func (e *Evals) resolveDataset(ctx context.Context) (*dataset, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	resp, err := e.do(ctx, "/api/public/v2/datasets/"+url.PathEscape(e.opts.Dataset), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("langfuse: no dataset named %q at %s; check the name and "+
			"LANGFUSE_HOST", e.opts.Dataset, e.endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("langfuse: the datasets API answered %s", e.redact(resp.Status))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRowBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("langfuse: reading the datasets response: %w", err)
	}
	if int64(len(body)) > maxRowBytes {
		return nil, fmt.Errorf("langfuse: the datasets response exceeded the %d-byte row cap", maxRowBytes)
	}

	ds, err := decodeDataset(body)
	if err != nil {
		return nil, fmt.Errorf("langfuse: decoding the datasets response: %w", err)
	}
	// The name query was ours (it is the path), so a dataset that answers
	// with a different name is a server that ignored the path — the
	// fingerprint must not depend on what the server decided to return.
	if ds.Name != "" && ds.Name != e.opts.Dataset {
		return nil, fmt.Errorf("langfuse: the datasets API answered for dataset %q when %q "+
			"was asked for; the fingerprint must not depend on a server that "+
			"ignores the path", ds.Name, e.opts.Dataset)
	}
	if ds.ID == "" {
		return nil, fmt.Errorf("langfuse: dataset %q came back without an id", e.opts.Dataset)
	}
	if ds.UpdatedAt == "" {
		return nil, fmt.Errorf("langfuse: dataset %q came back without an updatedAt; the "+
			"resume fingerprint is keyed on it", e.opts.Dataset)
	}
	return ds, nil
}

// Cases yields every item in the dataset, each mapped to a Case with its
// split assigned.
//
// Contract, per core.Evals:
//
//   - A yielded error is FATAL. A malformed row, a duplicate item id, an item
//     with a null input, or an oversized row stops the run — silently
//     dropping records would shrink the denominator behind every later delta
//     without anything showing it, and Langfuse itself would hand the record
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
func (e *Evals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	ds, err := e.resolveDataset(ctx)
	if err != nil {
		return nil, err
	}

	frac := e.opts.holdoutFrac()
	seed := e.opts.SplitSeed

	return func(yield func(*core.Case, error) bool) {
		// seen is scoped per iteration: a duplicate item id is fatal WITHIN
		// one iteration, and a re-read of the dataset may see the same ids
		// again.
		seen := make(map[string]struct{})

		for page := 1; ; page++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			if err := pageLimitError(page); err != nil {
				yield(nil, err)
				return
			}

			body, err := e.openPage(ctx, ds.Name, page)
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

				it, err := decodeItem(raw)
				if err != nil {
					yield(nil, err)
					return
				}
				if it.Archived {
					// Client-side filter: the API has no status query
					// parameter, so ARCHIVED items are excluded here rather
					// than asked away.
					continue
				}
				if it.ID == "" {
					// Fatal, not defaulted — the dev/holdout split is keyed on
					// the id, so a row without one could not stay stable
					// across re-reads. Same reasoning as jsonl's missing-id
					// fatal.
					yield(nil, fmt.Errorf("langfuse: item has no id, and the dev/holdout "+
						"split is keyed on it; give every item a stable id"))
					return
				}
				if _, dup := seen[it.ID]; dup {
					// Duplicate ids are fatal rather than tolerated: the split
					// is keyed on the id, so two rows sharing one would land
					// in the same half and be indistinguishable in every later
					// report. Same invariant jsonl and langsmith enforce
					// (docs/debt.md#45). Across a page seam this is a dataset
					// edited mid-pagination — a source defect to fix, not a
					// count to paper over.
					yield(nil, fmt.Errorf("langfuse: duplicate item id %q", it.ID))
					return
				}
				seen[it.ID] = struct{}{}

				provenance := &knov1.Provenance{
					Source:         "langfuse",
					SourceRef:      "dataset:" + ds.Name + ":" + it.ID,
					Derived:        it.Derived,
					DerivationNote: it.DerivationNote,
				}
				c := &core.Case{
					Id:         it.ID,
					Input:      it.Input,
					Expected:   it.Expected,
					Split:      split.AssignSplit(it.ID, seed, frac),
					Provenance: provenance,
				}
				if !yield(c, nil) {
					return
				}
			}

			// Release the page before opening the next one; the deferred
			// close still covers the early-break path.
			_ = body.Close()

			more, err := pr.morePages()
			if err != nil {
				yield(nil, err)
				return
			}
			if !more {
				return
			}
		}
	}, nil
}

// openPage fetches one page of dataset items and returns its body, capped so
// a page larger than the envelope cap is cut off loudly rather than buffered.
//
// datasetName is echoed into mid-pagination errors, so a run over several
// datasets can tell which one refused mid-stream.
func (e *Evals) openPage(ctx context.Context, datasetName string, page int) (io.ReadCloser, error) {
	q := url.Values{}
	q.Set("datasetName", datasetName)
	q.Set("page", strconv.Itoa(page))
	q.Set("limit", strconv.Itoa(pageSize))
	resp, err := e.do(ctx, "/api/public/dataset-items", q)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("langfuse: dataset %q: the dataset-items API answered %s",
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
// Provenance.Derived is an item harvested from a trace (sourceObservationId
// or sourceTraceId set), and the Run records the count at close so a
// weak-label eval cannot pass for a hand-authored one.
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
// It is deliberately NOT a content hash: hashing the items would require a
// full pass over a remote dataset, at remote latency, for a fingerprint that
// changes anyway when the dataset does. The dataset object from the
// resolution pass carries updatedAt, and any item edit bumps it — so host +
// dataset name + updatedAt has the same edit sensitivity at one request, no
// ordering hole, and no torn pass. A dataset renamed, recreated, or bulk-
// edited moves one of the three halves. An edit that leaves updatedAt
// untouched goes undetected; that is the accepted tradeoff, recorded in
// docs/plans/2026-08-29-langfuse-evals-adapter.md. When dataset version
// pinning lands (the plan's v0.2 deferral), the pinned version joins this
// hash.
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
	h := sha256.New()
	_, _ = io.WriteString(h, "langfuse:"+e.endpoint+":"+ds.Name+":"+ds.UpdatedAt)
	_, _ = h.Write(split.FingerprintSplit(e.opts.SplitSeed, e.opts.holdoutFrac()))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Evals = (*Evals)(nil)

// decodeDataset maps one v2 dataset object to the metadata this source needs.
func decodeDataset(raw []byte) (*dataset, error) {
	kvs, err := orderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("the dataset object is not a JSON object")
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
		case "updatedAt":
			if err := json.Unmarshal(kv.val, &ds.UpdatedAt); err != nil {
				return nil, fmt.Errorf("the dataset updatedAt is not a string")
			}
		}
	}
	return ds, nil
}
