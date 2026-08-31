package datasetserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Split is one config/split pair a dataset actually offers. A split is
// addressed by the pair, not by a name of its own, because Hugging Face
// datasets can expose the same split name under different configs.
type Split struct {
	// Config is the dataset configuration, "default" for the common case.
	Config string

	// Split is the split name, e.g. "train".
	Split string
}

// HasSplit reports whether a config/split pair appears in a split list.
// Callers use it after Splits to refuse a pair the dataset does not offer,
// naming the real list in the refusal.
func HasSplit(splits []Split, config, split string) bool {
	for _, s := range splits {
		if s.Config == config && s.Split == split {
			return true
		}
	}
	return false
}

// PairRefusal builds the refusal for a config/split pair the dataset does
// not offer, naming the real list so the fix does not require a second
// round-trip. The pair taxonomy lives here, with the 404 handling in
// OpenPage, because both adapters share it and a taxonomy written twice
// drifts.
func PairRefusal(dataset, config, split string, splits []Split) error {
	var offered []string
	for _, s := range splits {
		offered = append(offered, fmt.Sprintf("config %q split %q", s.Config, s.Split))
	}
	if len(offered) == 0 {
		return fmt.Errorf("dataset %q offers no config %q split %q; the dataset has "+
			"no splits at all — check the name", dataset, config, split)
	}
	return fmt.Errorf("dataset %q offers no config %q split %q; it offers %s. Pick "+
		"one of those, or fix the name", dataset, config, split, strings.Join(offered, ", "))
}

// Splits resolves a dataset's config/split list and the dataset's current
// revision (the x-revision header — see the package doc for why the header
// is the fingerprint and the revision query parameter is never used).
//
// The caller owns the pair check: Splits returns the real list, and the
// adapters refuse a pair not in it by naming the list. A 401 answers the
// name-or-gating question, and the refusal offers both remedies: the server
// sends 401 for a dataset that does not exist and for one that is gated or
// private, and the caller cannot know which from the status alone.
func (c *Client) Splits(ctx context.Context, dataset string) ([]Split, string, error) {
	q := url.Values{}
	q.Set("dataset", dataset)
	resp, err := c.do(ctx, "/splits", q)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		if c.token != "" {
			return nil, "", fmt.Errorf("the datasets-server API answered 401 for dataset %q, "+
				"which it sends both for a dataset that does not exist and for one that is gated; "+
				"check the name, and check that the HF_TOKEN it was given is current for this account", dataset)
		}
		return nil, "", fmt.Errorf("the datasets-server API answered 401 for dataset %q, "+
			"which it sends both for a dataset that does not exist and for one that is gated or private; "+
			"check the name, and set HF_TOKEN if the dataset is gated", dataset)
	case http.StatusOK:
	default:
		return nil, "", c.statusError(dataset, "", "", resp)
	}

	rev, err := c.revision(resp)
	if err != nil {
		return nil, "", err
	}
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, "", err
	}
	splits, err := decodeSplits(body)
	if err != nil {
		return nil, "", err
	}
	return splits, rev, nil
}

// decodeSplits parses the /splits envelope: {"splits": [{"config", "split"}...
// ]}. A missing or malformed entry is fatal — the list is what the pair
// refusal names, so a list that cannot be trusted cannot be used.
func decodeSplits(body []byte) ([]Split, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("the splits response is not a JSON object")
	}
	// Presence, not zero-value: a list that is "empty because the key was
	// missing" and a list that is "empty because the dataset has no splits"
	// are different answers, and the first one silently makes the pair
	// refusal name a list that was never sent.
	raw, ok := m["splits"]
	if !ok {
		return nil, fmt.Errorf("the splits response has no \"splits\" field")
	}
	var entries []struct {
		Config *string `json:"config"`
		Split  *string `json:"split"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("splits is not a JSON array")
	}
	out := make([]Split, 0, len(entries))
	for i, s := range entries {
		if s.Config == nil || *s.Config == "" {
			return nil, fmt.Errorf("the splits response entry %d has no config", i)
		}
		if s.Split == nil || *s.Split == "" {
			return nil, fmt.Errorf("the splits response entry %d has no split name", i)
		}
		out = append(out, Split{Config: *s.Config, Split: *s.Split})
	}
	return out, nil
}

// revision reads the x-revision header, refusing a response without one.
// The header is the fingerprint; a fingerprintless response cannot pin the
// split's identity, so it is fatal rather than treated as a plain response.
func (c *Client) revision(resp *http.Response) (string, error) {
	rev := resp.Header.Get("x-revision")
	if rev == "" {
		return "", fmt.Errorf("the datasets-server response carried no x-revision header; " +
			"the header is the fingerprint that pins the split's identity, and it is not optional")
	}
	return rev, nil
}

// statusError builds the refusal for a status outside the handled taxonomy.
// It never echoes the response body: bodies can carry anything, and the
// status is the part a human can act on.
func (c *Client) statusError(dataset, config, split string, resp *http.Response) error {
	if config == "" {
		return fmt.Errorf("the datasets-server API answered %s for dataset %q", resp.Status, dataset)
	}
	return fmt.Errorf("the datasets-server API answered %s for config %q split %q of dataset %q",
		resp.Status, config, split, dataset)
}
