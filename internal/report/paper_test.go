package report

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/broker"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/snapshot"
)

// Item 6 step 4's pass condition: a paper book on the wheel strategy renders
// from Alpaca's own response shapes, with no Robinhood code on its path. The
// fixture is a synthetic repo root (testdata/paper): one Alpaca account, a
// wheel book with no concentration caps, and one snapshot.
func paperFixture(t *testing.T) (*config.Book, *rules.Rules, snapshot.At) {
	t.Helper()
	root := filepath.Join("testdata", "paper")
	cfg, err := config.Load(root, "paper-wheel")
	if err != nil {
		t.Fatal(err)
	}
	r, err := rules.Load(filepath.Join(root, "books", "paper-wheel", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, r, snapshot.OnDisk(filepath.Join(root, "snapshots", "2026-11-10"))
}

func TestPaperBookRendersFromAlpacaShapes(t *testing.T) {
	cfg, r, at := paperFixture(t)
	if cfg.Source() != "alpaca" || len(r.ConcentrationCaps) != 0 {
		t.Fatalf("paper book: source %q, %d caps", cfg.Source(), len(r.ConcentrationCaps))
	}
	asOf := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

	// The snapshot is not Robinhood's: its decoder cannot read it.
	rh, err := broker.Lookup("robinhood")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join("testdata", "paper", "snapshots", "2026-11-10", "paper", "option_orders_all.json"))
	if err != nil {
		t.Fatal(err)
	}
	if os, err := rh.Orders("orders", body); err == nil && len(os) > 0 {
		t.Fatalf("test premise: the Robinhood decoder must not read Alpaca's orders, got %d", len(os))
	}

	var b bytes.Buffer
	code, err := Checkin(&b, cfg, r, at, asOf, false)
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if code != 0 {
		t.Errorf("check-in (exit %d):\n%s", code, out)
	}
	// Alpaca's positions carry no pending buys, so the lag check must not run.
	if strings.Contains(out, "pending_buy_lag") {
		t.Errorf("false pending_buy_lag on a source that reports no pending buys:\n%s", out)
	}
	for _, want := range []string{
		"AAA $55c 2027-06-17  3    $2.30  $1.15     $1.15  gtc", // the Rule 10 closer matches the live one
		"Open short-call credit (gross):  $690.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check-in lacks %q:\n%s", want, out)
		}
	}

	var bk bytes.Buffer
	if err := Book(&bk, cfg, r, at, asOf, "snapshots/2026-11-10"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bk.String(), "AAA") {
		t.Errorf("the book doesn't mention AAA:\n%s", bk.String())
	}
}

// A book that mixes sources is refused: a snapshot is read by one adapter.
func TestBookReadsOneSource(t *testing.T) {
	accts, err := config.ParseAccounts("a", []byte(`{"accounts":[
		{"name":"a","label":"A","last4":"1111","source":"alpaca","default_book":"b","modes":["manual"]},
		{"name":"c","label":"C","last4":"2222","source":"robinhood","default_book":"b","modes":["manual"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.ParseBook("b", "b.json", []byte(`{"strategy":"wheel","campaign_start":"2026-01-01","accounts":[{"name":"a","pool":"p"},{"name":"c","pool":"p"}]}`), accts)
	if err == nil || !strings.Contains(err.Error(), "one source") {
		t.Errorf("mixed-source book: %v", err)
	}
}
