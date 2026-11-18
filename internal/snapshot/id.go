package snapshot

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Market is the time zone snapshot ids are written in (exchange time).
var Market = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// ID is a snapshot folder name. It is the one place a snapshot's as-of date
// comes from, so every subcommand accepts exactly the same names (B1) and none
// falls back to the current clock (B3).
//
//	2026-11-10         a session (pre-market, close, or weekend pull)
//	2026-11-10T1012    intraday, 10:12 exchange time
//	2026-11-10T10:12   the same, with a colon
type ID struct {
	Name     string
	Date     time.Time // the session date at 00:00 UTC, like every other date in the tools: the as-of
	At       time.Time // the intraday time; zero for a date-only id
	Intraday bool
}

var idLayouts = []struct {
	layout   string
	intraday bool
}{
	{"2006-01-02", false},
	{"2006-01-02T1504", true},
	{"2006-01-02T15:04", true},
}

// IDFormats is the accepted list, for error messages.
const IDFormats = "YYYY-MM-DD, YYYY-MM-DDTHHMM or YYYY-MM-DDTHH:MM"

// ParseID reads the id from a snapshot directory's base name.
func ParseID(dir string) (ID, error) {
	name := filepath.Base(filepath.Clean(dir))
	for _, f := range idLayouts {
		t, err := time.ParseInLocation(f.layout, name, Market)
		if err != nil {
			continue
		}
		id := ID{Name: name, Date: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Intraday: f.intraday}
		if f.intraday {
			id.At = t
		}
		return id, nil
	}
	return ID{}, fmt.Errorf("%s: snapshot folders are named %s", dir, IDFormats)
}

// Before reports whether a is strictly earlier than b. A date-only id and an
// intraday id on the same session can't be ordered (a date-only id may be
// pre-market or after the close), so that pair, and equal ids, are errors.
func (a ID) Before(b ID) (bool, error) {
	if !a.Date.Equal(b.Date) {
		return a.Date.Before(b.Date), nil
	}
	if a.Intraday && b.Intraday && !a.At.Equal(b.At) {
		return a.At.Before(b.At), nil
	}
	if a.Intraday != b.Intraday {
		return false, fmt.Errorf("%s and %s are the same session and only one has a time; give both a time to compare them", a.Name, b.Name)
	}
	return false, fmt.Errorf("%s and %s are the same snapshot id", a.Name, b.Name)
}

// Sealed reports whether `ob snapshot` has written the folder's manifest.
// A sealed snapshot is read-only: nothing may write into it (B2).
func Sealed(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, manifestName))
	return err == nil
}
