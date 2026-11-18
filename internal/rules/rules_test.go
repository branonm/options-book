package rules

import (
	"github.com/branonm/options-book/internal/layout"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoRulesLoad(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "testdata", "synthetic", filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	if r.CoveredCallCloser.FractionOfFill.String() != "0.5" || r.CoveredCallCloser.TimeInForce != "gtc" {
		t.Errorf("Rule 10 closer = %s %s", r.CoveredCallCloser.FractionOfFill, r.CoveredCallCloser.TimeInForce)
	}
	// The synthetic book encodes no concentration cap.
	if len(r.ConcentrationCaps) != 0 {
		t.Errorf("the IRA wheel book carries caps: %+v", r.ConcentrationCaps)
	}
}

// concentration_caps is optional and may list several caps; every entry present
// must be complete, and names must be unique.
func TestConcentrationCaps(t *testing.T) {
	good, err := os.ReadFile(filepath.Join("..", "..", "testdata", "synthetic", filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	with := func(caps string) (*Rules, error) {
		text := strings.Replace(string(good), `"lot_size"`, `"concentration_caps": `+caps+",\n  \"lot_size\"", 1)
		p := filepath.Join(t.TempDir(), "rules.json")
		os.WriteFile(p, []byte(text), 0o644)
		return Load(p)
	}
	r, err := with(`[
		{"name": "group-a", "rule": 4, "symbols": ["AAA", "BBB"], "max_share_of_invested_equity": "0.50"},
		{"name": "group-b", "symbols": ["CCC", "AAA"], "max_share_of_invested_equity": "0.25"}]`)
	if err != nil || len(r.ConcentrationCaps) != 2 {
		t.Fatalf("two complete caps: %v", err)
	}
	if got := r.CapsFor("AAA"); len(got) != 2 {
		t.Errorf("AAA is in both groups, got %d", len(got))
	}
	if got := r.CapsFor("CCC"); len(got) != 1 || got[0].Name != "group-b" {
		t.Errorf("CCC caps = %+v", got)
	}
	if got := r.CapsFor("DDD"); len(got) != 0 {
		t.Errorf("DDD caps = %+v", got)
	}
	if r, err := with(`[]`); err != nil || len(r.ConcentrationCaps) != 0 {
		t.Errorf("an empty list means no caps: %v", err)
	}
	for name, caps := range map[string]string{
		"no name":        `[{"symbols": ["AAA"], "max_share_of_invested_equity": "0.5"}]`,
		"duplicate name": `[{"name": "a", "symbols": ["AAA"], "max_share_of_invested_equity": "0.5"}, {"name": "a", "symbols": ["BBB"], "max_share_of_invested_equity": "0.5"}]`,
		"no symbols":     `[{"name": "a", "symbols": [], "max_share_of_invested_equity": "0.5"}]`,
		"no max share":   `[{"name": "a", "symbols": ["AAA"]}]`,
		"zero max share": `[{"name": "a", "symbols": ["AAA"], "max_share_of_invested_equity": "0"}]`,
		"over 100%":      `[{"name": "a", "symbols": ["AAA"], "max_share_of_invested_equity": "1.5"}]`,
	} {
		if _, err := with(caps); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}

// A typo in rules.json must fail loudly, never silently disable a check.
func TestLoadRejectsBadValues(t *testing.T) {
	good, err := os.ReadFile(filepath.Join("..", "..", "testdata", "synthetic", filepath.FromSlash(layout.RulesFile(layout.DefaultBook))))
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string][2]string{
		"zero fraction":    {`"fraction_of_fill": "0.50"`, `"fraction_of_fill": "0"`},
		"misspelled key":   {`"fraction_of_fill"`, `"fraction_of_fil"`},
		"no tif":           {`"time_in_force": "gtc"`, `"time_in_force": ""`},
		"unknown rounding": {`"up_to_tick"`, `"bankers"`},
	} {
		bad := strings.Replace(string(good), edit[0], edit[1], 1)
		if bad == string(good) {
			t.Fatalf("%s: edit did not apply", name)
		}
		p := filepath.Join(t.TempDir(), "rules.json")
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: Load accepted it", name)
		}
	}
}
