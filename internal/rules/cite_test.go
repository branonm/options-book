package rules

import (
	"encoding/json"
	"github.com/branonm/options-book/internal/layout"
	"path/filepath"
	"testing"
)

func testDoc(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	const sample = `{
		"_doc": "a comment, never citable",
		"covered_call_closer": {
			"rule": 10,
			"status": "active, 10/28",
			"fraction_of_fill": "0.40",
			"time_in_force": "gtc"
		},
		"concentration_cap": {
			"rule": 4,
			"status": "active; split 9/30",
			"symbols": ["AAA", "BBB"],
			"max_share_of_invested_equity": "0.50"
		},
		"min_open_interest": {
			"rule": "Hard constraint 6",
			"value": 100,
			"confirmed": "Example Author, 2026-01-01"
		}
	}`
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sample), &doc); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return doc
}

func TestTopicsExcludesDocComment(t *testing.T) {
	topics := Topics(testDoc(t))
	want := []string{"concentration_cap", "covered_call_closer", "min_open_interest"}
	if len(topics) != len(want) {
		t.Fatalf("got %v, want %v", topics, want)
	}
	for i, w := range want {
		if topics[i] != w {
			t.Fatalf("got %v, want %v", topics, want)
		}
	}
}

func TestCiteIntRule(t *testing.T) {
	c, err := Cite(testDoc(t), "covered_call_closer")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Rule != "10" {
		t.Fatalf("got rule %q", c.Rule)
	}
	if c.Status != "active, 10/28" {
		t.Fatalf("got status %q", c.Status)
	}
	if c.Fields["fraction_of_fill"] != "0.40" || c.Fields["time_in_force"] != "gtc" {
		t.Fatalf("got fields %+v", c.Fields)
	}
}

func TestCiteStringRule(t *testing.T) {
	c, err := Cite(testDoc(t), "min_open_interest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Rule != "Hard constraint 6" {
		t.Fatalf("got rule %q", c.Rule)
	}
	if c.Fields["value"] != "100" {
		t.Fatalf("got fields %+v", c.Fields)
	}
	if c.Fields["confirmed"] != "Example Author, 2026-01-01" {
		t.Fatalf("got fields %+v", c.Fields)
	}
}

func TestCiteArrayField(t *testing.T) {
	c, err := Cite(testDoc(t), "concentration_cap")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Fields["symbols"] != `["AAA","BBB"]` {
		t.Fatalf("got symbols field %q", c.Fields["symbols"])
	}
}

func TestCiteUnknownTopic(t *testing.T) {
	if _, err := Cite(testDoc(t), "nonexistent"); err == nil {
		t.Fatal("expected an error for an unknown topic")
	}
}

func TestCiteRefusesDocComment(t *testing.T) {
	if _, err := Cite(testDoc(t), "_doc"); err == nil {
		t.Fatal("expected an error citing the _doc comment")
	}
}

func TestLoadRaw(t *testing.T) {
	doc, err := LoadRaw(filepath.Join("..", "..", "testdata", "synthetic", filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	topics := Topics(doc)
	if len(topics) == 0 {
		t.Fatal("expected at least one topic from the synthetic rules.json")
	}
	if _, err := Cite(doc, "max_risk"); err != nil {
		t.Fatalf("expected max_risk to be citable from the synthetic rules.json: %v", err)
	}
}

func TestCiteListEntry(t *testing.T) {
	doc := map[string]json.RawMessage{
		"concentration_caps": json.RawMessage(`[{"name": "group-a", "symbols": ["AAA"], "max_share_of_invested_equity": "0.50"}]`),
	}
	c, err := Cite(doc, "concentration_caps")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := `[{"name":"group-a","symbols":["AAA"],"max_share_of_invested_equity":"0.50"}]`; c.Fields["entries"] != want {
		t.Fatalf("got %q, want %q", c.Fields["entries"], want)
	}
}
