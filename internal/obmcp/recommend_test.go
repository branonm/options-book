package obmcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestWindowOf(t *testing.T) {
	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	lo, hi, ok := windowOf([]decimal.Decimal{d("54"), d("57.25"), decimal.Zero})
	// Lowest positive point 54 floors to 54; highest 57.25 × 1.30 = 74.425 ceils to 75.
	if !ok || !lo.Equal(d("54")) || !hi.Equal(d("75")) {
		t.Errorf("window = %s–%s ok=%v, want 54–75", lo, hi, ok)
	}
	lo, _, _ = windowOf([]decimal.Decimal{d("54.80"), d("60")})
	if !lo.Equal(d("54")) {
		t.Errorf("lo = %s, want 54 (floored)", lo)
	}
	if _, _, ok := windowOf([]decimal.Decimal{decimal.Zero}); ok {
		t.Error("no positive price should give no window")
	}
}

func TestExpirationsInWindow(t *testing.T) {
	var cb chainsBody
	body := `{"data":{"chains":[{"symbol":"BBB","expiration_dates":["2026-02-20","2026-02-27","2026-03-20","2026-03-20","2027-03-19"]},{"symbol":"CCC","expiration_dates":["2026-03-20"]}]}}`
	if err := json.Unmarshal([]byte(body), &cb); err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	// 2026-02-13 at 21:00 New York is already 02-14 in UTC; the window counts
	// from the New York date, so 2/27 is exactly 14 days out.
	now := time.Date(2026, 2, 13, 21, 0, 0, 0, ny)
	got := expirationsInWindow(cb, "BBB", now, 14, 240)
	if len(got) != 2 || got[0] != "2026-02-27" || got[1] != "2026-03-20" {
		t.Errorf("got %v, want [2026-02-27 2026-03-20] (2/20 too near, 2027 too far, CCC ignored, duplicate dropped)", got)
	}
	if got := expirationsInWindow(cb, "ZZZ", now, 14, 240); len(got) != 0 {
		t.Errorf("unknown symbol: %v", got)
	}
}
