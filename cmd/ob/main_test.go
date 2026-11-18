package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/branonm/options-book/internal/snapshot"
	"github.com/shopspring/decimal"
)

func TestUSD(t *testing.T) {
	cases := map[string]string{
		"8063":      "$8,063.00",
		"-196":      "-$196.00",
		"87.255":    "$87.26", // half away from zero
		"-0.001":    "$0.00",  // rounds to zero: no sign
		"1234567.8": "$1,234,567.80",
		"999.995":   "$1,000.00",
	}
	for in, want := range cases {
		if got := usd(decimal.RequireFromString(in)); got != want {
			t.Errorf("usd(%s) = %s, want %s", in, got, want)
		}
	}
}

// copyTree copies src into dst (files only, recursively).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fixtureAs lays out a scratch repo with the 2/13 synthetic snapshot under a new id.
func fixtureAs(t *testing.T, root, id string) string {
	t.Helper()
	dir := filepath.Join(root, "snapshots", id)
	copyTree(t, filepath.Join(repoDir, "snapshots", "2026-02-13"), dir)
	os.Remove(filepath.Join(dir, "manifest.json"))
	if _, err := os.Stat(filepath.Join(root, "history")); err != nil {
		copyTree(t, filepath.Join(repoDir, "history"), filepath.Join(root, "history"))
	}
	return dir
}

func quiet(t *testing.T) {
	t.Helper()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = null
	t.Cleanup(func() { os.Stdout = old; null.Close() })
}

// B1 + B3: every subcommand accepts the same snapshot ids, dates them by the
// id (never by the clock), and diff can order them.
func TestSnapshotIDsAcceptedByEverySubcommand(t *testing.T) {
	root := t.TempDir()
	ids := []string{"2026-02-13", "2026-02-13T1600", "2026-02-13T16:05"}
	var dirs []string
	for _, id := range ids {
		dirs = append(dirs, fixtureAs(t, root, id))
	}
	quiet(t)
	for _, dir := range dirs {
		asOf, err := resolveAsOf(dir, "")
		if err != nil || asOf.Format("2006-01-02") != "2026-02-13" {
			t.Errorf("%s: as-of %v, %v; want 2026-02-13", dir, asOf, err)
		}
		if err := runLedger([]string{"--snapshot", dir}); err != nil {
			t.Errorf("ledger %s: %v", dir, err)
		}
		if _, err := runCheckin([]string{"--snapshot", dir}); err != nil {
			t.Errorf("checkin %s: %v", dir, err)
		}
		if err := runBook([]string{"--snapshot", dir}); err != nil {
			t.Errorf("book %s: %v", dir, err)
		}
		if err := writableSnapshot(dir); err != nil {
			t.Errorf("history add --snapshot %s: %v", dir, err)
		}
	}
	// diff: the two intraday ids order by time.
	if err := runDiff([]string{"--from", dirs[1], "--to", dirs[2]}); err != nil {
		t.Errorf("diff intraday→intraday: %v", err)
	}
	for _, c := range []struct{ from, to string }{
		{dirs[2], dirs[1]}, // backwards
		{dirs[0], dirs[1]}, // date-only vs intraday, same session: ambiguous
		{dirs[1], dirs[1]}, // same id
	} {
		if err := runDiff([]string{"--from", c.from, "--to", c.to}); err == nil {
			t.Errorf("diff %s → %s: accepted, want an ordering error", filepath.Base(c.from), filepath.Base(c.to))
		}
	}
}

// B3: a folder that isn't a snapshot id is refused, not dated today.
func TestUnknownSnapshotNameIsRefused(t *testing.T) {
	for _, name := range []string{"latest", "2026-02-13-pm", "2026-02-13T2561", "11-07-2026"} {
		if _, err := resolveAsOf(filepath.Join("snapshots", name), ""); err == nil {
			t.Errorf("%s: accepted, want an error naming the formats", name)
		}
		if err := writableSnapshot(filepath.Join("snapshots", name)); err == nil {
			t.Errorf("history add --snapshot %s: accepted", name)
		}
	}
	if _, err := resolveAsOf(filepath.Join("snapshots", "latest"), "2026-02-13"); err != nil {
		t.Errorf("explicit --asof should still work: %v", err)
	}
}

// truncateLog keeps only the lines observed at or before until.
func truncateLog(t *testing.T, path, until string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var keep []byte
	for _, line := range bytes.SplitAfter(b, []byte("\n")) {
		var e struct {
			ObservedAt string `json:"observed_at"`
		}
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &e) != nil || e.ObservedAt > until {
			continue
		}
		keep = append(keep, line...)
	}
	if err := os.WriteFile(path, keep, 0o644); err != nil {
		t.Fatal(err)
	}
}

// B2: history add refuses a sealed snapshot, and refuses before it appends
// anything to the log.
func TestHistoryAddRefusesSealedSnapshot(t *testing.T) {
	pulls, err := filepath.Abs(filepath.Join("snapshots", "2026-02-13", "roth", "orders_pull"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := fixtureAs(t, root, "2026-02-13T1105")
	// Freeze the log as it stood at that pull; later pulls append to the real
	// one and would make this pull stale (B4).
	truncateLog(t, filepath.Join(root, "history", "roth", "orders.jsonl"), "2026-02-13T22:00:00Z")
	if _, err := snapshot.WriteManifest(dir, time.Date(2026, 2, 13, 22, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	logPath := filepath.Join("history", "roth", "orders.jsonl")
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	historyRef := filepath.Join(dir, "roth", "history.json")
	refBefore, _ := os.ReadFile(historyRef)
	// A pull observed later than the log with an extra order, so an add that
	// got past the guard would append.
	extra := filepath.Join(root, "new_order.json")
	page, _ := os.ReadFile(filepath.Join(pulls, "window_p1.json"))
	var resp map[string]any
	json.Unmarshal(page, &resp)
	orders := resp["data"].(map[string]any)["orders"].([]any)
	o := map[string]any{}
	for k, v := range orders[0].(map[string]any) {
		o[k] = v
	}
	o["id"] = "00000000-0000-0000-0000-00000000d002"
	o["created_at"] = "2026-02-13T15:00:00Z"
	resp["data"].(map[string]any)["orders"] = append([]any{o}, orders...)
	b, _ := json.Marshal(resp)
	os.WriteFile(extra, b, 0o644)

	quiet(t)
	err = runHistory([]string{"add", "--account", "roth", "--observed", "2026-02-13T22:30:00Z",
		"--created-since", "2026-02-06", "--pull", extra, "--pull", filepath.Join(pulls, "open_p1.json"), "--snapshot", dir})
	if err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("history add into a sealed snapshot: err = %v, want a refusal naming the seal", err)
	}
	after, _ := os.ReadFile(logPath)
	if string(after) != string(before) {
		t.Error("the log changed even though the add was refused")
	}
	if refAfter, _ := os.ReadFile(historyRef); string(refAfter) != string(refBefore) {
		t.Error("history.json in the sealed snapshot was rewritten")
	}
	if problems := snapshot.Check(dir, nil); len(problems) > 0 {
		t.Errorf("seal broken after refusal: %v", problems)
	}

	// The same add into an unsealed folder goes through.
	open := fixtureAs(t, root, "2026-02-13T1110")
	if err := runHistory([]string{"add", "--account", "roth", "--observed", "2026-02-13T22:30:00Z",
		"--created-since", "2026-02-06", "--pull", extra, "--pull", filepath.Join(pulls, "open_p1.json"), "--snapshot", open}); err != nil {
		t.Fatalf("history add into an unsealed snapshot: %v", err)
	}
}
