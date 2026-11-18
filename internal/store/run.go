package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Run is the storage design's audit record: every figure-producing tool call
// creates one, so a figure in a doc can be traced to the exact inputs — the
// snapshot, the rules.json and book.json in force, the server's commit — and
// to the exact output, by its sha256.
type Run struct {
	Tool        string
	Args        any // marshalled to canonical (key-sorted) JSON
	SnapshotID  string
	Book        string // the book the figure was computed for
	RulesSHA    string
	BookSHA     string
	AccountsSHA string // sha256 of the accounts.json in force
	Commit      string
	Output      []byte
	ExitCode    int
	RanAt       time.Time
	Inputs      []string // sha256 of the stored responses a figure was computed from, when not a snapshot's
}

// RecordRun creates the run document and returns its id. Ids are the UTC
// time plus 64 random bits, so they sort by time and never collide.
func RecordRun(ctx context.Context, db DB, r Run) (string, error) {
	// Round-tripping through a map sorts every object's keys.
	raw, err := json.Marshal(r.Args)
	if err != nil {
		return "", err
	}
	var canon any
	if err := json.Unmarshal(raw, &canon); err != nil {
		return "", err
	}
	args, err := json.Marshal(canon)
	if err != nil {
		return "", err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	at := r.RanAt.UTC()
	id := at.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(rnd[:])
	d := Doc{
		"tool": r.Tool, "args": string(args), "snapshot_id": r.SnapshotID,
		"book": r.Book, "accounts_sha256": r.AccountsSHA,
		"rules_sha256": r.RulesSHA, "book_sha256": r.BookSHA, "tool_commit": r.Commit,
		"output_sha256": sum(r.Output), "exit_code": int64(r.ExitCode), "ran_at": at.Format(time.RFC3339Nano),
	}
	if len(r.Inputs) > 0 {
		d["inputs"] = strings.Join(r.Inputs, ",")
	}
	if err := db.Create(ctx, CRun, id, d); err != nil {
		return "", fmt.Errorf("recording the run: %w", err)
	}
	return id, nil
}

// SHA256 is the hex sha256 of b, as the run record and the manifests write it.
func SHA256(b []byte) string { return sum(b) }

// Summary is the one-line result `ob db verify` prints and the verify tool
// returns, so the two are byte-identical.
func (r *VerifyReport) Summary(spec string) string {
	return fmt.Sprintf("Verified %s: %d snapshots (%d sealed), %d bodies, %d order versions, every hash, chain and seal; export → import → export is byte-identical (%d bytes).\n",
		spec, r.Snapshots, r.Sealed, r.Bodies, r.OrderVersions, r.ExportBytes)
}
