package snapshot

import (
	"testing"
	"testing/fstest"

	_ "github.com/branonm/options-book/internal/broker/alpaca"
	"github.com/branonm/options-book/internal/layout"
)

// Alpaca keeps open interest on the contract, not the quote. LoadOptionAt joins
// the two so a ticket sees the contract's figure (not 0).
func TestLoadOptionJoinsOpenInterestFromContract(t *testing.T) {
	const id = "AAA270617C00055000"
	base := "snapshots/2026-11-10/" + layout.OptionFolder(id) + "/"
	fsys := fstest.MapFS{
		base + layout.QuoteFile: {Data: []byte(`{"snapshots":{"` + id + `":{"latestQuote":{"ap":2.27,"bp":2.17,"t":"2026-11-10T15:30:00Z"},"greeks":{"delta":0.297}}}}`)},
		base + layout.InstrumentFile: {Data: []byte(`{"option_contracts":[{"symbol":"` + id + `","status":"active","tradable":true,` +
			`"expiration_date":"2027-06-17","underlying_symbol":"AAA","type":"call","strike_price":"55","size":"100","open_interest":"14460"}],"next_page_token":null}`)},
	}
	o, err := LoadOptionAt(At{FS: fsys, Dir: "snapshots/2026-11-10", Broker: "alpaca"}, id)
	if err != nil {
		t.Fatal(err)
	}
	if o.Quote.OpenInterest != 14460 {
		t.Errorf("open interest = %d, want 14460 from the contract", o.Quote.OpenInterest)
	}
}

// With no open_interest on the contract the quote stays 0: unknown stays visible
// (a ticket then rejects on Rule open interest) and nothing is invented.
func TestLoadOptionNoContractOpenInterest(t *testing.T) {
	const id = "AAA270617C00055000"
	base := "snapshots/2026-11-10/" + layout.OptionFolder(id) + "/"
	fsys := fstest.MapFS{
		base + layout.QuoteFile: {Data: []byte(`{"snapshots":{"` + id + `":{"latestQuote":{"ap":2.27,"bp":2.17,"t":"2026-11-10T15:30:00Z"}}}}`)},
		base + layout.InstrumentFile: {Data: []byte(`{"option_contracts":[{"symbol":"` + id + `","status":"active","tradable":true,` +
			`"expiration_date":"2027-06-17","underlying_symbol":"AAA","type":"call","strike_price":"55","size":"100"}],"next_page_token":null}`)},
	}
	o, err := LoadOptionAt(At{FS: fsys, Dir: "snapshots/2026-11-10", Broker: "alpaca"}, id)
	if err != nil {
		t.Fatal(err)
	}
	if o.Quote.OpenInterest != 0 {
		t.Errorf("open interest = %d, want 0", o.Quote.OpenInterest)
	}
}
