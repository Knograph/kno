package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// recordFixtures fetches every source live, trims them, and writes the
// fixtures plus note.txt into dir. The fixtures must be REAL captures —
// structure intact, only script/style and whitespace removed — because the
// tests' claim is "this is what the page looked like on the capture date".
// The sha256 in note.txt covers the FULL untrimmed capture, so anyone can
// verify the fixture is the real page. A fetch failure is reported loudly
// and aborts: a hand-built "good" fixture would defeat the detector's
// purpose, and silence here would let one through.
func recordFixtures(dir string) error {
	// Fetch and trim EVERY source before writing anything: a failed mid-way
	// fetch must not leave a half-overwritten committed fixture set, and the
	// sha256 in note.txt must always name the captures actually on disk.
	type capture struct {
		file   string
		url    string
		body   []byte
		sha256 string
	}
	var caps []capture
	for _, s := range sources {
		body, err := fetch(s.url)
		if err != nil {
			return fmt.Errorf("recording %s: %w", s.name, err)
		}
		sum := sha256.Sum256(body)
		var trimmed []byte
		switch s.name {
		case "openrouter":
			trimmed, err = minifyJSON(body)
		case "bedrock":
			trimmed, err = extractBedrockRegions(body)
		default:
			trimmed, err = trimHTML(body)
		}
		if err != nil {
			return fmt.Errorf("trimming %s: %w", s.name, err)
		}
		caps = append(caps, capture{file: s.file, url: s.url, body: trimmed, sha256: hex.EncodeToString(sum[:])})
	}

	// Stage in a temp dir, then move into place: a write failure at any point
	// leaves the committed fixture set exactly as it was.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dir, ".staging-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }() // best-effort: files were already renamed out

	var note strings.Builder
	fmt.Fprintf(&note, "# kno pricingcheck fixture captures\n# recorded %s\n\n", time.Now().UTC().Format(time.RFC3339))
	for _, c := range caps {
		if err := os.WriteFile(filepath.Join(staging, c.file), c.body, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(&note, "%s\n  url: %s\n  sha256: %s\n\n", c.file, c.url, c.sha256)
	}
	if err := os.WriteFile(filepath.Join(staging, "note.txt"), []byte(note.String()), 0o600); err != nil {
		return err
	}

	for _, c := range caps {
		if err := os.Rename(filepath.Join(staging, c.file), filepath.Join(dir, c.file)); err != nil {
			return err
		}
	}
	return os.Rename(filepath.Join(staging, "note.txt"), filepath.Join(dir, "note.txt"))
}

// trimHTML removes script, style, noscript, comments, and the head, and
// collapses text whitespace, then re-renders. Structure is untouched: tables
// keep their rows, anchors keep their hrefs, and the parser the checks use
// will read the trimmed capture exactly as it read the live page.
func trimHTML(body []byte) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			drop := c.Type == html.CommentNode ||
				(c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style" || c.Data == "noscript" || c.Data == "head"))
			if drop {
				n.RemoveChild(c)
			} else {
				if c.Type == html.TextNode {
					c.Data = strings.Join(strings.Fields(c.Data), " ")
				}
				walk(c)
			}
			c = next
		}
	}
	walk(doc)
	var b bytes.Buffer
	if err := html.Render(&b, doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// minifyJSON strips whitespace outside strings. Strings are preserved
// byte-for-byte — the OpenRouter prices are decimal strings and must not be
// re-encoded through a float on the way to the fixture.
func minifyJSON(body []byte) ([]byte, error) {
	var b bytes.Buffer
	inStr, esc := false, false
	for _, c := range body {
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			b.WriteByte(c)
		case ' ', '\t', '\n', '\r':
			// strip
		default:
			b.WriteByte(c)
		}
	}
	var v interface{}
	if err := json.Unmarshal(b.Bytes(), &v); err != nil {
		return nil, fmt.Errorf("minified fixture does not parse: %w", err)
	}
	return b.Bytes(), nil
}

// extractBedrockRegions reduces AWS's raw offers index to the shape the
// regional check reads: region -> usagetype -> price. The raw index keys
// prices by opaque SKUs across two top-level maps; only the region, the
// usage type, and the USD price per 1K tokens survive. Rows whose
// locationType is not "AWS Region" (the deprecated edge rows) and usage
// types that do not name Anthropic inference are dropped — the check's
// subject is the regional spread of Claude inference on Bedrock, not the
// platform's auxiliary fees.
func extractBedrockRegions(body []byte) ([]byte, error) {
	var o struct {
		Products map[string]struct {
			Attributes struct {
				Region       string `json:"region"`
				LocationType string `json:"locationType"`
				UsageType    string `json:"usagetype"`
			} `json:"attributes"`
		} `json:"products"`
		Terms map[string]map[string]map[string]struct {
			PriceDimensions map[string]struct {
				PricePerUnit map[string]string `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"terms"`
	}
	if err := json.Unmarshal(body, &o); err != nil {
		return nil, err
	}
	regions := make(map[string]map[string]rateEntry)
	for sku, p := range o.Products {
		a := p.Attributes
		if a.LocationType != "AWS Region" || !strings.HasPrefix(a.UsageType, "AnthropicClaude") {
			continue
		}
		onDemand, ok := o.Terms["OnDemand"][sku]
		if !ok {
			continue
		}
		for _, term := range onDemand {
			for _, dim := range term.PriceDimensions {
				price, ok := dim.PricePerUnit["USD"]
				if !ok {
					continue
				}
				m := regions[a.Region]
				if m == nil {
					m = make(map[string]rateEntry)
					regions[a.Region] = m
				}
				if prev, dup := m[a.UsageType]; dup && prev.Price != price {
					return nil, fmt.Errorf("bedrock: usagetype %s in %s carries two different prices (%s vs %s); the extraction would lose data",
						a.UsageType, a.Region, prev, price)
				}
				m[a.UsageType] = rateEntry{Price: price}
			}
		}
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("bedrock: no AnthropicClaude inference rows in the offers index")
	}
	out, err := json.Marshal(struct {
		Regions map[string]map[string]rateEntry `json:"regions"`
	}{Regions: regions})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// rateEntry is the fixture's per-code price object; the extraction emits the
// same shape the parser reads, so a recorded fixture and a hand-authored one
// are interchangeable.
type rateEntry struct {
	Price string `json:"price"`
}
