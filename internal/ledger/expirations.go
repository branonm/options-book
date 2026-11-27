package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/shopspring/decimal"
)

// Expiration is how one contract finished: the underlying's regular-session
// close on the expiration date. In the money means assigned (calls: close >
// strike; puts: close < strike); OCC auto-exercises contracts a cent or more
// in the money.
type Expiration struct {
	Contract string          `json:"contract"` // ContractKey.String()
	Symbol   string          `json:"symbol"`
	Type     string          `json:"type"`
	Strike   decimal.Decimal `json:"strike"`
	Expiry   string          `json:"expiry"`
	Close    decimal.Decimal `json:"underlying_close"`
	Outcome  string          `json:"outcome"` // "assigned" or "expired_worthless", derived and checked on load
	Source   string          `json:"source"`
}

// Outcome derives assigned / expired_worthless from the close.
func (e Expiration) derive() string {
	itm := (e.Type == "call" && e.Close.GreaterThan(e.Strike)) || (e.Type == "put" && e.Close.LessThan(e.Strike))
	if itm {
		return EndAssigned
	}
	return EndWorthless
}

// ExpirationsFile is history/expirations.json.
type ExpirationsFile struct {
	Doc       string       `json:"_doc"`
	Contracts []Expiration `json:"contracts"`
}

// LoadExpirations reads the file; a missing file is an empty map. Every
// record's stored outcome must agree with its close and strike.
func LoadExpirations(path string) (map[string]Expiration, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Expiration{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseExpirations(path, b)
}

// LoadExpirationsFS is LoadExpirations over a read-only Source view.
func LoadExpirationsFS(fsys fs.FS, name string) (map[string]Expiration, error) {
	b, err := fs.ReadFile(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Expiration{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseExpirations(name, b)
}

// ParseExpirations decodes and checks history/expirations.json; path names it in errors.
func ParseExpirations(path string, b []byte) (map[string]Expiration, error) {
	out := map[string]Expiration{}
	var f ExpirationsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range f.Contracts {
		key := ContractKey{Symbol: e.Symbol, Type: e.Type, Strike: e.Strike.String(), Expiry: e.Expiry}.String()
		if e.Contract != key {
			return nil, fmt.Errorf("%s: record %q does not match its fields (%s)", path, e.Contract, key)
		}
		if _, err := time.Parse("2006-01-02", e.Expiry); err != nil || !e.Close.IsPositive() {
			return nil, fmt.Errorf("%s: %s needs an expiry date and a positive close", path, key)
		}
		if d := e.derive(); e.Outcome != d {
			return nil, fmt.Errorf("%s: %s says %s but a close of %s against a %s strike means %s", path, key, e.Outcome, e.Close, e.Strike, d)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("%s: %s recorded twice", path, key)
		}
		out[key] = e
	}
	return out, nil
}

// NewExpiration builds a record from a close.
func NewExpiration(k ContractKey, close decimal.Decimal, source string) (Expiration, error) {
	strike, err := decimal.NewFromString(k.Strike)
	if err != nil {
		return Expiration{}, err
	}
	e := Expiration{Contract: k.String(), Symbol: k.Symbol, Type: k.Type, Strike: strike, Expiry: k.Expiry, Close: close, Source: source}
	e.Outcome = e.derive()
	return e, nil
}

func resolveExpiration(l *Leg, exps map[string]Expiration) {
	e, ok := exps[l.Key.String()]
	if !ok {
		return
	}
	l.End = e.Outcome
	if l.End == EndAssigned {
		l.Note = fmt.Sprintf("assigned at expiration: %s closed %s vs %s strike", e.Symbol, e.Close, e.Strike)
	} else {
		l.Note = fmt.Sprintf("expired worthless: %s closed %s vs %s strike", e.Symbol, e.Close, e.Strike)
	}
}

// callAwayReset returns the day after the latest full call-away on a symbol:
// a call assigned at expiry while no other call on that symbol was open
// across the same expiration (the book writes whole blocks, so every share
// went). A partial assignment cannot be split from the order record; it
// returns a warning instead, and block_starts in config/book.json settles it.
func callAwayReset(legs []Leg, symbol string) (string, string) {
	reset := ""
	for _, a := range legs {
		if a.Key.Symbol != symbol || a.Key.Type != "call" || a.End != EndAssigned || a.PreCampaign {
			continue
		}
		exp := a.Key.Expiry
		for _, o := range legs {
			if o.Key.Symbol != symbol || o.Key.Type != "call" || o.PreCampaign || (o.OptionID == a.OptionID && o.OpenedAt == a.OpenedAt) {
				continue
			}
			openAcross := o.OpenedAt <= exp && (o.End == EndOpen || o.ClosedAt > exp)
			if openAcross {
				return "", fmt.Sprintf("%s %s was assigned while %s was still open: a partial call-away. Block premium is not reset; set block_starts for %s in config/book.json if the block changed",
					symbol, a.Key, o.Key, symbol)
			}
		}
		if d, err := time.Parse("2006-01-02", exp); err == nil {
			if r := d.AddDate(0, 0, 1).Format("2006-01-02"); r > reset {
				reset = r
			}
		}
	}
	return reset, ""
}

// ExpirationsDoc is the _doc line history/expirations.json is written with.
// The store Source regenerates the file with it, so it must not change
// without re-importing.
const ExpirationsDoc = "How contracts that ran through expiration without a closing order finished: the underlying's regular-session close on the expiration date. In the money = assigned. Append-only; written by `wheel expirations`."
