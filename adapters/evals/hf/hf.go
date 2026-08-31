package hf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"iter"
	"net/http"
	"os"
	"strings"

	"github.com/knograph/kno/adapters/evals/split"
	"github.com/knograph/kno/adapters/internal/datasetserver"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TokenEnv is the environment variable holding the credential, when the
// dataset is gated or private. Environment-only by design: there is
// deliberately no field that would put a key value on a command line, where
// it lands in shell history, ps output, and CI logs.
const TokenEnv = "HF_TOKEN"

// Options configures a Hugging Face Evals source.
type Options struct {
	// Dataset is the dataset to read, as org/name.
	//
	// The dataset is resolved at each iteration, so an edit to the dataset
	// shows up in the resume fingerprint rather than being silently
	// absorbed.
	Dataset string

	// Config is the dataset configuration to read, e.g. "main". Required:
	// the config is a fact of the dataset, not something to guess.
	Config string

	// Split is the split name to read, e.g. "train". Required, same reason.
	Split string

	// Token is the bearer credential. Empty reads HF_TOKEN, then nothing —
	// a public dataset needs none, and a gated one without a token answers
	// 401 with both remedies.
	Token string

	// Host is the datasets-server root. Empty reads DefaultHost. A
	// plain-http or private-address host is refused unless the Allow*
	// fields below opt in, because the token travels there. Self-hosted
	// mirrors of datasets-server are the reason these exist.
	Host string

	// HoldoutFrac is the share of Cases held back. Zero means
	// split.DefaultHoldoutFrac, exactly as the jsonl adapter maps it — the
	// dev-set size must not vary with the source.
	HoldoutFrac float64

	// SplitSeed deliberately re-splits an eval set. Normally empty; recorded
	// on the Run and part of the resume fingerprint.
	SplitSeed string

	// AllowInsecureBaseURL permits a plain-HTTP host. The token rides the
	// connection in the clear otherwise, so the refusal is the default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 endpoints, for a
	// self-hosted mirror on the local network. Link-local is never
	// permitted — 169.254.169.254 is the instance-metadata endpoint.
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

// Evals reads Cases from a Hugging Face dataset split.
//
// It satisfies core.Evals. Callers that must not see the holdout wrap it
// with core.Seal, which is a distinct type — so a stage that forgets does
// not compile.
type Evals struct {
	opts   Options
	client *datasetserver.Client
}

// New returns an Evals reading from the named split.
//
// The dataset is not fetched here. Each call to Cases resolves the name and
// streams the rows, so every iteration gets an independent cursor — which is
// what lets the conformance harness run several iterations over one source
// and what lets a resumed run re-read from the start.
func New(opts Options) (*Evals, error) {
	if opts.Dataset == "" {
		return nil, fmt.Errorf("hf: no dataset name given; name it in --evals as " +
			"hf:<org>/<name>/<config>/<split>")
	}
	if opts.Config == "" {
		return nil, fmt.Errorf("hf: no config given for dataset %q; name it in --evals as "+
			"hf:<org>/<name>/<config>/<split>", opts.Dataset)
	}
	if opts.Split == "" {
		return nil, fmt.Errorf("hf: no split given for dataset %q config %q; name it in "+
			"--evals as hf:<org>/<name>/<config>/<split>", opts.Dataset, opts.Config)
	}
	if opts.HoldoutFrac < 0 || opts.HoldoutFrac >= 1 {
		return nil, fmt.Errorf("hf: holdout fraction %v must be in [0, 1)", opts.HoldoutFrac)
	}

	token := opts.Token
	if token == "" {
		token = os.Getenv(TokenEnv)
	}
	client, err := datasetserver.New(datasetserver.Options{
		Host:                 opts.Host,
		Token:                token,
		AllowInsecureBaseURL: opts.AllowInsecureBaseURL,
		AllowPrivateAddress:  opts.AllowPrivateAddress,
		HTTPClient:           opts.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("hf: %w", err)
	}
	return &Evals{opts: opts, client: client}, nil
}

// resolveSplit resolves the dataset and reads the x-revision fingerprint,
// refusing a config/split pair the dataset does not offer — naming the real
// list, so the fix does not require a second round-trip.
func (e *Evals) resolveSplit(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	splits, rev, err := e.client.Splits(ctx, e.opts.Dataset)
	if err != nil {
		return "", err
	}
	if !datasetserver.HasSplit(splits, e.opts.Config, e.opts.Split) {
		return "", datasetserver.PairRefusal(e.opts.Dataset, e.opts.Config, e.opts.Split, splits)
	}
	return rev, nil
}

// openSplit performs every open-time refusal in one place, so Cases and
// CountSplits share one failure story: the 401 taxonomy, the pair absence,
// the first page's 404/partial/revision answers, and the column inventory.
func (e *Evals) openSplit(ctx context.Context) (*openedSplit, error) {
	rev, err := e.resolveSplit(ctx)
	if err != nil {
		return nil, err
	}
	page0, err := e.client.OpenPage(ctx, e.opts.Dataset, e.opts.Config, e.opts.Split, 0)
	if err != nil {
		return nil, err
	}
	if page0.Revision != rev {
		return nil, fmt.Errorf("hf: the dataset's fingerprint moved between the split "+
			"resolution (x-revision %q) and the first page (x-revision %q); the split "+
			"changed while it was being opened", rev, page0.Revision)
	}
	cols := discoverColumns(page0.Rows)
	if cols.input == "" && len(page0.Rows) > 0 {
		return nil, fmt.Errorf("hf: dataset %q config %q split %q has no input column — "+
			"none of input, prompt, question. An eval Case needs an input. The row's "+
			"columns are: %s", e.opts.Dataset, e.opts.Config, e.opts.Split,
			strings.Join(actualColumns(page0.Rows[0]), ", "))
	}
	return &openedSplit{anchor: rev, cols: cols, first: page0}, nil
}

// openedSplit is the open-time state every iteration of the iterator starts
// from.
type openedSplit struct {
	anchor string // x-revision of the first page; every later page must match
	cols   columns
	first  *datasetserver.Page
}

// Cases yields every row of the split, each mapped to a Case with its split
// assigned.
//
// Contract, per core.Evals:
//
//   - A yielded error is FATAL. A null input, a duplicate row_idx, a page
//     whose x-revision drifted from the anchor, or a partial subsample stops
//     the run — silently dropping records would shrink the denominator
//     behind every later delta without anything showing it.
//   - Cleanup is deferred INSIDE the closure. Each page is read fully and
//     closed before its rows are yielded, so there is no resource to leak
//     across an early break.
//   - ctx is checked before each yield.
//   - Yielded Cases are borrowed for one iteration; each is fresh, so
//     cloning before retaining is always safe.
//
// The dataset resolves on every call, so a renamed or deleted dataset is a
// hard error here — reported from Cases itself, with nothing yielded —
// rather than a silently different eval set. An empty split yields zero
// Cases and is legal: "exists and is empty" is a different claim from "no
// such split".
func (e *Evals) Cases(ctx context.Context) (iter.Seq2[*core.Case, error], error) {
	open, err := e.openSplit(ctx)
	if err != nil {
		return nil, err
	}

	frac := e.opts.holdoutFrac()
	seed := e.opts.SplitSeed
	return func(yield func(*core.Case, error) bool) {
		// seen is scoped per iteration: a duplicate row_idx is fatal WITHIN
		// one iteration, and a re-read of the dataset may see the same rows
		// again.
		seen := make(map[string]struct{})

		offset := int64(0)
		for page := 1; page <= datasetserver.MaxPages; page++ {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			var rows []datasetserver.Row
			if page == 1 {
				rows = open.first.Rows
			} else {
				p, err := e.client.OpenPage(ctx, e.opts.Dataset, e.opts.Config, e.opts.Split, offset)
				if err != nil {
					yield(nil, err)
					return
				}
				if p.Revision != open.anchor {
					yield(nil, fmt.Errorf("hf: the dataset changed while it was being read: "+
						"the first page answered x-revision %q and a later page %q; the split "+
						"is no longer one object, so the fingerprint cannot pin it", open.anchor, p.Revision))
					return
				}
				rows = p.Rows
			}
			if len(rows) == 0 {
				return
			}

			for _, row := range rows {
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}
				c, err := mapRow(row, open.cols, e.opts.Dataset, e.opts.Config, e.opts.Split)
				if err != nil {
					yield(nil, err)
					return
				}
				if _, dup := seen[c.GetId()]; dup {
					yield(nil, fmt.Errorf("hf: row %d was served twice across pages of config "+
						"%q split %q; an in-run duplicate is fatal so the denominator stays "+
						"honest", row.RowIdx, e.opts.Config, e.opts.Split))
					return
				}
				seen[c.GetId()] = struct{}{}
				c.Split = split.AssignSplit(c.GetId(), seed, frac)
				if !yield(c, nil) {
					return
				}
			}
			offset += int64(len(rows))
		}
		yield(nil, fmt.Errorf("hf: pagination exceeded %d pages; the split keeps issuing "+
			"fresh rows, so the fetch stops here rather than spin", datasetserver.MaxPages))
	}, nil
}

// CountSplits reads the split and reports how it divides, without yielding
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
		switch c.GetSplit() {
		case knov1.Split_SPLIT_DEV:
			counts.Dev++
		case knov1.Split_SPLIT_HOLDOUT:
			counts.Holdout++
		}
	}
	// WeakLabelCases stays zero: HF rows carry no per-record derivation
	// note, so nothing here is ever marked derived. See the package doc.
	return counts, nil
}

// ContentHash fingerprints the split for the resume fingerprint.
//
// It is sha256 over the four identity facts — dataset, config, split, and
// the x-revision the server answered — plus the split assignment. The
// revision query parameter is deliberately absent: the server ignores it,
// and the header is the only thing that pins what the server actually
// served. The dataset and its revision are resolved at each call, so a
// changed dataset is a changed fingerprint, not a silently different eval
// set under an identical resume key.
func (e *Evals) ContentHash(ctx context.Context) (string, error) {
	rev, err := e.resolveSplit(ctx)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "hf:%s:%s:%s:%s", e.opts.Dataset, e.opts.Config, e.opts.Split, rev)
	_, _ = h.Write(split.FingerprintSplit(e.opts.SplitSeed, e.opts.holdoutFrac()))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// locator is the human-readable source reference for a row.
func locator(dataset, config, split string, rowIdx int64) string {
	return fmt.Sprintf("%s/%s/%s@%d", dataset, config, split, rowIdx)
}
