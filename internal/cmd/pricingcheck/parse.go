package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/net/html"
)

// anthropicHeader is the exact header of the price table on Anthropic's
// pricing page, as a committed literal in DECODED form (the page writes
// "Cache Hits &amp; Refreshes"; the HTML parser hands back
// "Cache Hits & Refreshes").
//
// Selection is exact equality against the full column set, including BOTH
// cache-write columns. The literal is what pins the page layout: a table
// whose header matches this string is the table, and zero or two matches
// mean the page was restructured and every price on it is suspect.
var anthropicHeader = []string{
	"Model", "Base Input Tokens", "5m Cache Writes", "1h Cache Writes",
	"Cache Hits & Refreshes", "Output Tokens",
}

// anthropicColumns maps the header literal's columns to row fields. Model and
// the five rates, in header order.
var anthropicColumns = []string{"model", "input", "cacheWrite5m", "cacheWrite1h", "cachedRead", "output"}

// anthropicFastHeader is the fast-mode table's header: model, input, output.
// Fast mode publishes no cache rates, matching the table's presence rule —
// nil, "not billed separately".
var anthropicFastHeader = []string{"Model", "Input", "Output"}

// parseOpenRouter reads the OpenRouter model list.
//
// The envelope is a contract: an object whose "data" is an array, each item
// carrying a string "id" and a "pricing" object with "prompt" and "completion"
// decimal strings in USD per TOKEN. A wrong envelope is errShape — an
// unreachable source — not an empty one: an empty list would read as "nothing
// to check", which is exactly the failure mode that silently approves.
//
// Only ids under the two provider namespaces Kno serves adapters for are
// kept. A model behind a base URL (tencent/..., mistral/...) is deliberately
// absent from the table and is not something the detector is asked to judge.
func parseOpenRouter(body []byte) ([]row, error) {
	var env struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("%w: decoding envelope: %v", errShape, err)
	}
	if len(env.Data) == 0 {
		return nil, fmt.Errorf("%w: envelope has no data", errShape)
	}
	out := make([]row, 0, len(env.Data))
	for _, m := range env.Data {
		if strings.TrimSpace(m.ID) == "" {
			return nil, fmt.Errorf("%w: model with an empty id", errShape)
		}
		in, err := microsPerMTokFromUSDPerToken(m.Pricing.Prompt)
		if err != nil {
			return nil, fmt.Errorf("%w: %s pricing.prompt: %v", errShape, m.ID, err)
		}
		outv, err := microsPerMTokFromUSDPerToken(m.Pricing.Completion)
		if err != nil {
			return nil, fmt.Errorf("%w: %s pricing.completion: %v", errShape, m.ID, err)
		}
		scheme, model, ok := splitProviderID(m.ID)
		if !ok {
			continue // base-URL provider; not a Kno scheme, nothing to judge
		}
		out = append(out, row{
			source: "openrouter", scheme: scheme, model: model,
			canonical: canonicalModel(model),
			input:     in, output: outv,
		})
	}
	return out, nil
}

// splitProviderID splits an OpenRouter id into the provider namespace and the
// model. "openai/gpt-5.6-sol" -> ("openai", "gpt-5.6-sol"). An id without the
// slash is not a provider-scoped id at all.
func splitProviderID(id string) (scheme, model string, ok bool) {
	i := strings.Index(id, "/")
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// parseAnthropic reads the price table off Anthropic's pricing page.
//
// Selection is the committed header literal: every table on the page is
// parsed, and the one whose header matches anthropicHeader exactly is the
// price table. Zero or more than one match is errSelect — a restructured page
// that the gate (check 2) must fail on. A malformed row inside the matched
// table is errRow, which check 3 gates on.
func parseAnthropic(body []byte) ([]row, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: parsing html: %v", errShape, err)
	}
	var tables [][][]string
	walkNodes(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "table" {
			tables = append(tables, parseTable(n))
		}
	})
	var matched [][][]string
	for _, t := range tables {
		if len(t) > 0 && headerEqual(t[0], anthropicHeader) {
			matched = append(matched, t)
		}
	}
	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("%w: no table has the committed header %q", errSelect, strings.Join(anthropicHeader, " | "))
	case 1:
	default:
		return nil, fmt.Errorf("%w: %d tables match the committed header literal", errSelect, len(matched))
	}
	table := matched[0]
	out := make([]row, 0, len(table)-1)
	for i, cells := range table[1:] {
		if len(cells) != len(anthropicHeader) {
			return nil, fmt.Errorf("%w: row %d has %d cells, want %d", errRow, i+1, len(cells), len(anthropicHeader))
		}
		model := strings.TrimSpace(stripParenthetical(cells[0]))
		if model == "" {
			return nil, fmt.Errorf("%w: row %d has an empty model name", errRow, i+1)
		}
		rates := make([]*big.Rat, 5)
		for j, c := range cells[1:] {
			r, err := microsPerMTokFromUSDPerMTok(c)
			if err != nil {
				return nil, fmt.Errorf("%w: row %d column %s: %v", errRow, i+1, anthropicColumns[j+1], err)
			}
			rates[j] = r
		}
		out = append(out, row{
			source: "anthropic", scheme: "anthropic",
			model: model, canonical: canonicalModel(model),
			input: rates[0], cacheWrite5m: rates[1], cacheWrite1h: rates[2],
			cachedRead: rates[3], output: rates[4],
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: table has no data rows", errShape)
	}
	fast, err := parseAnthropicFast(tables)
	if err != nil {
		return nil, err
	}
	return append(out, fast...), nil
}

// parseAnthropicFast reads the fast-mode table, selected by its own header
// literal. Selection is as gated as the main table's: fast rows price
// variants that authorize real spend (docs/debt.md#46's rows), so a page
// that stops publishing them is a restructure to fail on, not to ignore.
//
// The fast table names two models per row ("Claude Opus 5 / Claude Opus
// 4.8"), so each row splits into one row per model. The id is the page's own
// naming convention carried to its API spelling: the canonicalized model
// name with "-fast" appended, which is exactly the table key
// (claude-opus-5-fast). Constructing the variant id here — rather than
// matching the page's prose name — is what lets the agreement check compare
// these rows against OpenRouter's variant ids.
func parseAnthropicFast(tables [][][]string) ([]row, error) {
	var matched [][][]string
	for _, t := range tables {
		if len(t) > 0 && headerEqual(t[0], anthropicFastHeader) {
			matched = append(matched, t)
		}
	}
	switch len(matched) {
	case 0:
		return nil, fmt.Errorf("%w: no table has the committed fast-mode header %q", errSelect, strings.Join(anthropicFastHeader, " | "))
	case 1:
	default:
		return nil, fmt.Errorf("%w: %d tables match the committed fast-mode header literal", errSelect, len(matched))
	}
	table := matched[0]
	var out []row
	for i, cells := range table[1:] {
		if len(cells) != len(anthropicFastHeader) {
			return nil, fmt.Errorf("%w: fast-mode row %d has %d cells, want %d", errRow, i+1, len(cells), len(anthropicFastHeader))
		}
		in, err := microsPerMTokFromUSDPerMTok(cells[1])
		if err != nil {
			return nil, fmt.Errorf("%w: fast-mode row %d input: %v", errRow, i+1, err)
		}
		outRate, err := microsPerMTokFromUSDPerMTok(cells[2])
		if err != nil {
			return nil, fmt.Errorf("%w: fast-mode row %d output: %v", errRow, i+1, err)
		}
		for _, name := range strings.Split(cells[0], "/") {
			name = strings.TrimSpace(stripParenthetical(name))
			if name == "" {
				return nil, fmt.Errorf("%w: fast-mode row %d has an empty model name", errRow, i+1)
			}
			id := canonicalModel(name) + "-fast"
			out = append(out, row{
				source: "anthropic", scheme: "anthropic",
				model: id, canonical: id,
				input: in, output: outRate,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: fast-mode table has no data rows", errShape)
	}
	return out, nil
}

// parseOpenAI reads the per-model prices off OpenAI's model comparison page.
//
// The page has no tables. Each model card is anchored by an <a> whose href is
// /api/docs/models/<id>; inside the same card a section header reads "Pricing
// Per 1M tokens", and under it sit three label rows — "Input", "Cached Input",
// "Output" — each followed by a sibling holding the price in USD per 1M
// tokens. Selection is by label TEXT and sibling position, never by CSS class:
// class names on this page are build-scrambled and change on every deploy.
//
// The page publishes no cache-write rate, matching the table's presence rule:
// cacheWrite stays nil, "not billed separately" rather than "free".
func parseOpenAI(body []byte) ([]row, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: parsing html: %v", errShape, err)
	}
	type priced struct {
		model string
		rates [3]*big.Rat // input, cachedRead, output
	}
	var out []priced
	var cur *priced
	walkNodes(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "a" {
			if id, ok := openAIModelFromHref(attr(n, "href")); ok {
				cur = &priced{model: id}
				out = append(out, *cur)
				return
			}
		}
		if n.Data == "div" && openAIPricingHeader(n) && cur != nil {
			if r, ok := openAIPricingRows(n); ok {
				last := out[len(out)-1]
				last.rates = r
				out[len(out)-1] = last
			}
		}
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no model anchors found", errShape)
	}
	rows := make([]row, 0, len(out))
	for _, p := range out {
		if p.rates == [3]*big.Rat{} {
			return nil, fmt.Errorf("%w: model %s has no pricing section", errShape, p.model)
		}
		rows = append(rows, row{
			source: "openai", scheme: "openai", model: p.model, canonical: canonicalModel(p.model),
			input: p.rates[0], cachedRead: p.rates[1], output: p.rates[2],
		})
	}
	return rows, nil
}

// openAIModelFromHref extracts a model id from a comparison-page anchor.
func openAIModelFromHref(href string) (string, bool) {
	const prefix = "/api/docs/models/"
	if !strings.HasPrefix(href, prefix) {
		return "", false
	}
	id := href[len(prefix):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// openAIPricingHeader reports whether n is the "Pricing / Per 1M tokens"
// header row that opens a model's pricing section. The header is one row
// whose two direct children read "Pricing" and "Per 1M tokens". The page is
// minified — no whitespace between sibling tags — so comparing each child's
// own text is the reliable text anchor, not the row's collapsed text.
func openAIPricingHeader(n *html.Node) bool {
	var parts []string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			parts = append(parts, textOf(c))
		}
	}
	return len(parts) == 2 && parts[0] == "Pricing" && parts[1] == "Per 1M tokens"
}

// openAIPricingRows extracts the three label-price rows beneath a pricing
// header. The header row and the section content container are siblings
// inside one wrapper; the label rows are the content container's direct
// children. Each label row carries its price in a sibling element, and the
// price must parse as USD per MTok. All three labels must appear exactly
// once, or the section is a restructured page and the parse fails.
func openAIPricingRows(header *html.Node) ([3]*big.Rat, bool) {
	wrapper := header.Parent
	if wrapper == nil {
		return [3]*big.Rat{}, false
	}
	for c := wrapper.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c == header {
			continue
		}
		var rates [3]*big.Rat
		var counts [3]int
		for r := c.FirstChild; r != nil; r = r.NextSibling {
			if r.Type != html.ElementNode {
				continue
			}
			label, priceEl := pricingRowParts(r)
			idx := openAILabelIndex(label)
			if idx < 0 {
				continue
			}
			counts[idx]++
			if counts[idx] == 1 {
				rate, err := microsPerMTokFromUSDPerMTok(textOf(priceEl))
				if err != nil {
					break
				}
				rates[idx] = rate
			}
		}
		if counts == [3]int{1, 1, 1} {
			return rates, true
		}
	}
	return [3]*big.Rat{}, false
}

// openAILabelIndex maps a pricing row label to its rate slot: input, cached
// read, output. Anything else is not a price row.
func openAILabelIndex(label string) int {
	switch label {
	case "Input":
		return 0
	case "Cached Input":
		return 1
	case "Output":
		return 2
	}
	return -1
}

// pricingRowParts splits a pricing row into its label and price elements. A
// row is <div><div>Input</div><div class="font-semibold">...price...</div></div>
// — two direct children; the price text is whatever the second one holds.
func pricingRowParts(n *html.Node) (label string, price *html.Node) {
	children := 0
	var first, second *html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		children++
		if children == 1 {
			first = c
		} else if children == 2 {
			second = c
			break
		}
	}
	if children != 2 {
		return "", nil
	}
	return textOf(first), second
}

// walkNodes visits n and every descendant in document order.
func walkNodes(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkNodes(c, fn)
	}
}

// attr returns an element attribute's value.
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// textOf returns an element's descendant text with runs of whitespace
// collapsed to single spaces and surrounding space trimmed.
func textOf(n *html.Node) string {
	var b strings.Builder
	walkNodes(n, func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
		}
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

// rowCells returns the texts of a row's direct td/th children, in order.
func rowCells(tr *html.Node) []string {
	var cells []string
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && (c.Data == "td" || c.Data == "th") {
			cells = append(cells, textOf(c))
		}
	}
	return cells
}

// parseTable reads a table as rows of cell texts. Rows are direct tr
// children or nested one level under thead/tbody, the structure the
// anthropic page uses.
func parseTable(n *html.Node) [][]string {
	var out [][]string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if c.Data == "tr" {
			out = append(out, rowCells(c))
			continue
		}
		if c.Data == "thead" || c.Data == "tbody" {
			for r := c.FirstChild; r != nil; r = r.NextSibling {
				if r.Type == html.ElementNode && r.Data == "tr" {
					out = append(out, rowCells(r))
				}
			}
		}
	}
	return out
}

// headerEqual compares two header rows exactly, cell for cell.
func headerEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
