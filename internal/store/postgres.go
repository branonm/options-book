package store

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a DB over one PostgreSQL table (docs/cloud-sql-design.md). Like
// Firestore it only ever creates a document, reads one, or lists a
// collection: no update or delete statement exists here, and the schema
// refuses them too (a trigger for everyone, and a server user that is granted
// SELECT and INSERT only).
//
// A document is one row. Its fields are stored as jsonb with a type tag per
// value, {"s": "text"}, {"i": "123"}, {"b": true} or {"x": "<base64>"}, so an
// int64 never becomes a float and a []byte comes back exactly.
type Postgres struct {
	pool    *pgxpool.Pool
	onClose func() // releases whatever the pool was dialled through (the Cloud SQL connector)
}

//go:embed postgres_schema.sql
var postgresSchema string

// NewPostgres connects with a libpq-style DSN. A non-empty schema is put first
// on the connection's search_path, which is how tests get a store of their own.
func NewPostgres(ctx context.Context, dsn, schema string) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres DSN: %w", err)
	}
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize()
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// Close releases the connections.
func (p *Postgres) Close() {
	p.pool.Close()
	if p.onClose != nil {
		p.onClose()
	}
}

// Migrate creates the table, the trigger that refuses UPDATE, DELETE and
// TRUNCATE, and, when serverRole is not empty, grants that role SELECT and
// INSERT on it and nothing else. It is run by the table's owner and is safe to
// run again.
func (p *Postgres) Migrate(ctx context.Context, serverRole string) error {
	if _, err := p.pool.Exec(ctx, postgresSchema); err != nil {
		return fmt.Errorf("postgres schema: %w", err)
	}
	if serverRole == "" {
		return nil
	}
	if _, err := p.pool.Exec(ctx, "GRANT SELECT, INSERT ON doc TO "+pgx.Identifier{serverRole}.Sanitize()); err != nil {
		return fmt.Errorf("postgres grant to %s: %w", serverRole, err)
	}
	return nil
}

func (p *Postgres) Create(ctx context.Context, coll, id string, d Doc) error {
	if err := checkID(coll, id); err != nil {
		return err
	}
	if err := checkDoc(d); err != nil {
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	b, err := encodePGFields(d)
	if err != nil {
		return fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	tag, err := p.pool.Exec(ctx,
		`INSERT INTO doc (coll, id, fields) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, coll, id, b)
	switch {
	case err != nil:
		return fmt.Errorf("%s/%s: create: %w", coll, id, err)
	case tag.RowsAffected() == 0:
		return fmt.Errorf("%s/%s: %w", coll, id, ErrExists)
	}
	return nil
}

func (p *Postgres) Get(ctx context.Context, coll, id string) (Doc, error) {
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT fields FROM doc WHERE coll = $1 AND id = $2`, coll, id).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("%s/%s: %w", coll, id, fs.ErrNotExist)
	case err != nil:
		return nil, fmt.Errorf("%s/%s: get: %w", coll, id, err)
	}
	d, err := decodePGFields(raw)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", coll, id, err)
	}
	return d, nil
}

// List sorts in byte order (COLLATE "C"), the order Mem and Firestore give.
func (p *Postgres) List(ctx context.Context, coll string) ([]Item, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, fields FROM doc WHERE coll = $1 ORDER BY id COLLATE "C"`, coll)
	if err != nil {
		return nil, fmt.Errorf("%s: list: %w", coll, err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("%s: list: %w", coll, err)
		}
		d, err := decodePGFields(raw)
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", coll, id, err)
		}
		out = append(out, Item{ID: id, Doc: d})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: list: %w", coll, err)
	}
	return out, nil
}

func encodePGFields(d Doc) ([]byte, error) {
	out := make(map[string]map[string]any, len(d))
	for k, v := range d {
		switch v := v.(type) {
		case string:
			out[k] = map[string]any{"s": v}
		case int64:
			out[k] = map[string]any{"i": strconv.FormatInt(v, 10)}
		case bool:
			out[k] = map[string]any{"b": v}
		case []byte:
			out[k] = map[string]any{"x": base64.StdEncoding.EncodeToString(v)}
		default:
			return nil, fmt.Errorf("field %s: unsupported type %T", k, v)
		}
	}
	return json.Marshal(out)
}

func decodePGFields(raw []byte) (Doc, error) {
	var m map[string]struct {
		S *string `json:"s"`
		I *string `json:"i"`
		B *bool   `json:"b"`
		X *string `json:"x"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	out := make(Doc, len(m))
	for k, v := range m {
		switch {
		case v.S != nil:
			out[k] = *v.S
		case v.I != nil:
			n, err := strconv.ParseInt(*v.I, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			out[k] = n
		case v.B != nil:
			out[k] = *v.B
		case v.X != nil:
			b, err := base64.StdEncoding.DecodeString(*v.X)
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", k, err)
			}
			out[k] = b
		default:
			return nil, fmt.Errorf("field %s: no value tag", k)
		}
	}
	return out, nil
}

// scratchPrefix names the schemas tests create. Their names end in the time
// they were made, so a later run can remove the ones a crashed run left behind.
const scratchPrefix = "ob_test_"

// ScratchPostgres makes a store of its own inside the database the DSN names:
// a fresh schema, migrated, with no server role. It is for tests (the DSN's
// user must be allowed to create schemas); the returned function drops the
// schema. Scratch schemas older than an hour are removed first.
func ScratchPostgres(ctx context.Context, dsn string) (*Postgres, func(), error) {
	admin, err := NewPostgres(ctx, dsn, "")
	if err != nil {
		return nil, nil, err
	}
	defer admin.Close()
	if err := admin.reapScratch(ctx); err != nil {
		return nil, nil, err
	}
	schema := fmt.Sprintf("%s%d_%d", scratchPrefix, time.Now().UnixNano(), os.Getpid())
	if _, err := admin.pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return nil, nil, fmt.Errorf("postgres scratch schema: %w", err)
	}
	drop := func() {
		a, err := NewPostgres(context.Background(), dsn, "")
		if err != nil {
			return
		}
		defer a.Close()
		a.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	}
	p, err := NewPostgres(ctx, dsn, schema)
	if err != nil {
		drop()
		return nil, nil, err
	}
	if err := p.Migrate(ctx, ""); err != nil {
		p.Close()
		drop()
		return nil, nil, err
	}
	return p, func() { p.Close(); drop() }, nil
}

func (p *Postgres) reapScratch(ctx context.Context) error {
	rows, err := p.pool.Query(ctx, `SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE 'ob\_test\_%'`)
	if err != nil {
		return err
	}
	var old []string
	cutoff := time.Now().Add(-time.Hour).UnixNano()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		stamp, _, _ := strings.Cut(strings.TrimPrefix(name, scratchPrefix), "_")
		if n, err := strconv.ParseInt(stamp, 10, 64); err == nil && n < cutoff {
			old = append(old, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range old {
		if _, err := p.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			return err
		}
	}
	return nil
}

// CheckAppendOnly tries UPDATE, DELETE and TRUNCATE as the connected user and
// returns an error naming any the database allowed. Each is tried in a
// transaction that is always rolled back, so even a statement that is allowed
// changes nothing. Run it as the server's user to show the grants hold
// (docs/cloud-sql-design.md, step 3). With an empty table the trigger layer has
// no row to refuse, so only the privilege layer is exercised.
func (p *Postgres) CheckAppendOnly(ctx context.Context) error {
	var allowed []string
	for _, stmt := range []string{
		`UPDATE doc SET fields = fields`,
		`DELETE FROM doc`,
		`TRUNCATE doc`,
	} {
		tx, err := p.pool.Begin(ctx)
		if err != nil {
			return err
		}
		_, execErr := tx.Exec(ctx, stmt)
		if err := tx.Rollback(ctx); err != nil {
			return fmt.Errorf("rolling back %q: %w", stmt, err)
		}
		if execErr == nil {
			allowed = append(allowed, strings.Fields(stmt)[0])
		}
	}
	if len(allowed) > 0 {
		return fmt.Errorf("the database allowed %s (each was rolled back; nothing changed)", strings.Join(allowed, ", "))
	}
	return nil
}
