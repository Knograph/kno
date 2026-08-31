package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/knograph/kno/adapters/internal/datasetserver"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
)

// TokenEnv is the environment variable holding the credential, when the
// dataset is gated or private. Environment-only by design: there is
// deliberately no field that would put a key value on a command line, where
// it lands in shell history, ps output, and CI logs.
const TokenEnv = "HF_TOKEN"

// Options configures a Hugging Face Pool source.
type Options struct {
	// Dataset is the dataset to read, as org/name.
	Dataset string

	// Config is the dataset configuration to read, e.g. "main". Required:
	// the config is a fact of the dataset, not something to guess.
	Config string

	// Split is the split name to read, e.g. "train". Required, same reason.
	Split string

	// Kind is the Asset kind every row becomes: "knowledge" or "behavior".
	// Required — see the package doc for why an hf pool without a declared
	// kind is refused.
	Kind string

	// Token is the bearer credential. Empty reads HF_TOKEN, then nothing —
	// a public dataset needs none.
	Token string

	// Host is the datasets-server root. Empty reads DefaultHost. A
	// plain-http or private-address host is refused unless the Allow*
	// fields below opt in, because the token travels there.
	Host string

	// AllowInsecureBaseURL permits a plain-HTTP host. The token rides the
	// connection in the clear otherwise, so the refusal is the default.
	AllowInsecureBaseURL bool

	// AllowPrivateAddress permits loopback and RFC1918 endpoints, for a
	// self-hosted mirror on the local network. Link-local is never
	// permitted.
	AllowPrivateAddress bool

	// HTTPClient supplies transport and TLS settings only; its redirect
	// policy and timeout are always overwritten.
	HTTPClient *http.Client
}

// kindOf maps the declared spelling onto the enum, closed and exact. The
// empty spelling is refused, not defaulted: KIND_UNSPECIFIED would route
// every Asset by guess, and the declared kind is the whole point of the
// hf:...:<kind> grammar.
func kindOf(s string) (knov1.Kind, error) {
	switch s {
	case "knowledge":
		return knov1.Kind_KIND_KNOWLEDGE, nil
	case "behavior":
		return knov1.Kind_KIND_BEHAVIOR, nil
	default:
		return knov1.Kind_KIND_UNSPECIFIED, fmt.Errorf(
			`unknown kind %q; declare the kind in --pool as hf:<org>/<name>/`+
				`<config>/<split>:knowledge or :behavior`, s,
		)
	}
}

// Pool reads Assets from a Hugging Face dataset split.
//
// It satisfies core.Pool.
type Pool struct {
	opts   Options
	kind   knov1.Kind
	client *datasetserver.Client
}

// New returns a Pool reading from the named split.
//
// The dataset is not fetched here; each call to Assets resolves the split
// and streams the rows, so every iteration gets an independent cursor.
func New(opts Options) (*Pool, error) {
	if opts.Dataset == "" {
		return nil, fmt.Errorf("hf: no dataset name given; name it in --pool as " +
			"hf:<org>/<name>/<config>/<split>:<kind>")
	}
	if opts.Config == "" {
		return nil, fmt.Errorf("hf: no config given for dataset %q; name it in --pool as "+
			"hf:<org>/<name>/<config>/<split>:<kind>", opts.Dataset)
	}
	if opts.Split == "" {
		return nil, fmt.Errorf("hf: no split given for dataset %q config %q; name it in "+
			"--pool as hf:<org>/<name>/<config>/<split>:<kind>", opts.Dataset, opts.Config)
	}
	kind, err := kindOf(opts.Kind)
	if err != nil {
		return nil, fmt.Errorf("hf: %w", err)
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
	return &Pool{opts: opts, kind: kind, client: client}, nil
}

// resolveSplit resolves the dataset and reads the x-revision fingerprint,
// refusing a config/split pair the dataset does not offer.
func (p *Pool) resolveSplit(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	splits, rev, err := p.client.Splits(ctx, p.opts.Dataset)
	if err != nil {
		return "", err
	}
	if !datasetserver.HasSplit(splits, p.opts.Config, p.opts.Split) {
		return "", datasetserver.PairRefusal(p.opts.Dataset, p.opts.Config, p.opts.Split, splits)
	}
	return rev, nil
}

// openSplit performs every open-time refusal in one place: the 401
// taxonomy, the pair absence, the first page's 404/partial/revision
// answers, and the dataset-level text-column decision.
func (p *Pool) openSplit(ctx context.Context) (*openedSplit, error) {
	rev, err := p.resolveSplit(ctx)
	if err != nil {
		return nil, err
	}
	page0, err := p.client.OpenPage(ctx, p.opts.Dataset, p.opts.Config, p.opts.Split, 0)
	if err != nil {
		return nil, err
	}
	if page0.Revision != rev {
		return nil, fmt.Errorf("hf: the dataset's fingerprint moved between the split "+
			"resolution (x-revision %q) and the first page (x-revision %q); the split "+
			"changed while it was being opened", rev, page0.Revision)
	}
	hasText := false
	for _, row := range page0.Rows {
		content, err := composeContent(row)
		if err == nil && len(content) > 0 {
			hasText = true
			break
		}
	}
	return &openedSplit{anchor: rev, first: page0, hasText: hasText}, nil
}

// openedSplit is the open-time state every iteration of the iterator starts
// from.
type openedSplit struct {
	anchor string // x-revision of the first page; every later page must match
	first  *datasetserver.Page
	// hasText is the dataset-level text decision. A split whose first page
	// carries no text-bearing column is an EMPTY pool, not an error: a pool
	// of numbers is legal, and this flag is what lets a later text-free row
	// stay fatal while the whole-split case stays legal.
	hasText bool
}

// Assets yields every row of the split, each mapped to an Asset.
//
// Contract, per core.Pool (identical to core.Evals.Cases):
//
//   - A yielded error is FATAL. A row with no text-bearing column, a
//     duplicate row id, or a page whose x-revision drifted stops the run.
//   - Cleanup is deferred INSIDE the closure. Each page is read fully and
//     closed before its rows are yielded, so there is no resource to leak
//     across an early break.
//   - ctx is checked before each yield.
//   - Yielded Assets are borrowed for one iteration; each is fresh, so
//     cloning before retaining is always safe.
//
// A split whose first page has no text-bearing column at all yields zero
// Assets and is legal: a pool of numbers is an empty pool, not an error. A
// LATER row with no text-bearing column, in a split that does have text, is
// fatal — the split promised text and delivered a row without it.
func (p *Pool) Assets(ctx context.Context) (iter.Seq2[*core.Asset, error], error) {
	open, err := p.openSplit(ctx)
	if err != nil {
		return nil, err
	}
	if !open.hasText {
		// The whole first page has no text-bearing column: a legal empty
		// pool. Yield nothing — an empty pool is a different claim from a
		// broken one.
		return func(func(*core.Asset, error) bool) {}, nil
	}

	return func(yield func(*core.Asset, error) bool) {
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
				pge, err := p.client.OpenPage(ctx, p.opts.Dataset, p.opts.Config, p.opts.Split, offset)
				if err != nil {
					yield(nil, err)
					return
				}
				if pge.Revision != open.anchor {
					yield(nil, fmt.Errorf("hf: the dataset changed while it was being read: "+
						"the first page answered x-revision %q and a later page %q; the split "+
						"is no longer one object, so the fingerprint cannot pin it", open.anchor, pge.Revision))
					return
				}
				rows = pge.Rows
			}
			if len(rows) == 0 {
				return
			}

			for _, row := range rows {
				if err := ctx.Err(); err != nil {
					yield(nil, err)
					return
				}
				asset, err := p.assetAt(row, seen)
				if err != nil {
					yield(nil, err)
					return
				}
				if !yield(asset, nil) {
					return
				}
			}
			offset += int64(len(rows))
		}
		yield(nil, fmt.Errorf("hf: pagination exceeded %d pages; the split keeps issuing "+
			"fresh rows, so the fetch stops here rather than spin", datasetserver.MaxPages))
	}, nil
}

// assetAt maps one row to an Asset, refusing a duplicate id and a row
// without text.
func (p *Pool) assetAt(row datasetserver.Row, seen map[string]struct{}) (*core.Asset, error) {
	content, err := composeContent(row)
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		// Only fatal when the split has text to begin with: the first page's
		// text decision is openSplit's, so a LATER row that promised and
		// broke the shape is the fatal one.
		return nil, fmt.Errorf("hf: row %d has no text-bearing column; every asset in "+
			"config %q split %q carries the text of its row, and this row has none",
			row.RowIdx, p.opts.Config, p.opts.Split)
	}

	id := fmt.Sprintf("%s/%s/%s@%d", p.opts.Dataset, p.opts.Config, p.opts.Split, row.RowIdx)
	if _, dup := seen[id]; dup {
		return nil, fmt.Errorf("hf: asset id %q was served twice across pages of config %q "+
			"split %q; an in-run duplicate is fatal so the denominator stays honest",
			id, p.opts.Config, p.opts.Split)
	}
	seen[id] = struct{}{}

	return &core.Asset{
		Id:      id,
		Content: content,
		Kind:    p.kind,
		// The address declared the kind, so the report can tell an asserted
		// routing decision from a measured one — always true here, and kept
		// as an explicit field because the csv pool's equivalent is
		// conditional.
		UserOverridden: true,
		Cost:           &knov1.CostVector{ContextTokens: contextTokens(len(content))},
		Provenance: &knov1.Provenance{
			Source:    "hf",
			SourceRef: id,
			// IngestedAt is deliberately unset: it would make two reads of
			// an unchanged split produce different Assets, so a pool that
			// had not moved would look changed on every read.
		},
	}, nil
}

// composeContent renders a row's text-bearing columns as sorted
// "name: value" lines.
//
// A column is text-bearing when its value is a JSON string; null, numbers,
// booleans, and structured values are not. The lines are sorted by column
// name so the bytes are deterministic whatever order the server emits keys
// in, and every column is labeled so the content survives without its
// header row.
func composeContent(row datasetserver.Row) ([]byte, error) {
	// RawMessage, not []byte: the values are arbitrary JSON, and a map of
	// []byte would try to base64-decode the string values.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row.Row, &fields); err != nil {
		return nil, fmt.Errorf("hf: row %d is not a JSON object", row.RowIdx)
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var lines []string
	for _, name := range names {
		raw := fields[name]
		if len(raw) == 0 || raw[0] != '"' {
			continue // not a JSON string: not text-bearing
		}
		value, isNull, err := datasetserver.ValueString(raw)
		if err != nil || isNull {
			continue // ValueString cannot err on a literal string, and null is not text
		}
		lines = append(lines, name+": "+value)
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// bytesPerToken is the divisor behind contextTokens. See its godoc for why
// it is this number and not the one the reservation path uses.
const bytesPerToken = 3.6

// contextTokens estimates what carrying this Asset adds to every request.
//
// This is the RANKING denominator of delta_per_cost, and it must not be the
// reservation path's countTokens (adapters/agent/pricing): that deliberately
// over-counts by about 3x on prose and takes a model argument, so feeding it
// in here would rank the portfolio by content type instead of by value. The
// Hugging Face pool inherits the same estimate as the markdown and CSV
// pools: bytes over a fixed divisor, centered on the one measurement in
// this tree — English prose at 3.6 bytes/token (docs/debt.md#68). Rounded
// up, so a non-empty Asset never costs zero tokens: delta_per_cost over a
// zero denominator is an infinity, and an infinity sorts to the top of a
// greedy ranking.
func contextTokens(sizeBytes int) int64 {
	return int64(math.Ceil(float64(sizeBytes) / bytesPerToken))
}

// Compile-time proof that this adapter satisfies the Ring-0 contract.
var _ core.Pool = (*Pool)(nil)
