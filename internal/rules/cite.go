package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Citation is one rules.json entry, formatted for an agent to quote as the
// source for a numeric claim. It is built from the raw JSON, not the typed
// Rules struct, so a new rules.json entry is citable before anything in
// this package decodes it.
type Citation struct {
	Topic  string            `json:"topic"`
	Rule   string            `json:"rule,omitempty"`
	Status string            `json:"status,omitempty"`
	Fields map[string]string `json:"fields"`
}

// LoadRaw reads rules.json as a generic document, for citation only. It
// does not validate the file the way Load does — Load is still the only
// way to get a *Rules the rest of the tools trust.
func LoadRaw(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("rules: parsing %s: %w", path, err)
	}
	return doc, nil
}

// Topics lists the citable entries in doc (every top-level key except the
// "_doc" comment), sorted.
func Topics(doc map[string]json.RawMessage) []string {
	topics := make([]string, 0, len(doc))
	for k := range doc {
		if k == "_doc" {
			continue
		}
		topics = append(topics, k)
	}
	sort.Strings(topics)
	return topics
}

// Cite extracts topic from doc. "rule" and "status" are pulled out as named
// fields when present; everything else in that entry becomes a Fields entry,
// each rendered as compact text rather than deep-flattened.
func Cite(doc map[string]json.RawMessage, topic string) (*Citation, error) {
	raw, ok := doc[topic]
	if !ok || topic == "_doc" {
		return nil, fmt.Errorf("rules: no entry %q in rules.json (see Topics for the valid names)", topic)
	}
	// A list entry (concentration_caps) is cited as one "entries" field, one object per element.
	if t := bytes.TrimSpace(raw); len(t) > 0 && t[0] == '[' {
		return &Citation{Topic: topic, Fields: map[string]string{"entries": stringify(raw)}}, nil
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, fmt.Errorf("rules: entry %q is not an object: %w", topic, err)
	}

	c := &Citation{Topic: topic, Fields: map[string]string{}}
	for k, v := range entry {
		s := stringify(v)
		switch k {
		case "rule":
			c.Rule = s
		case "status":
			c.Status = s
		default:
			c.Fields[k] = s
		}
	}
	return c, nil
}

// stringify renders a raw JSON value as plain text: a quoted string loses
// its quotes, everything else (numbers, bools, arrays, nested objects) is
// compacted (whitespace removed) but otherwise kept in its JSON form.
func stringify(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
