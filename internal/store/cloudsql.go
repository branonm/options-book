package store

import (
	"context"
	"fmt"
	"net"
	"strings"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewCloudSQL connects to a Cloud SQL for PostgreSQL instance through the Cloud
// SQL Go connector, signing in with IAM database authentication: no password
// exists anywhere. instance is the connection name, "project:region:instance".
// user is the IAM database user: for a service account, its email without the
// ".gserviceaccount.com" suffix (see CloudSQLUser).
//
// The connector reaches the instance's public address, which has no authorised
// networks, using the credentials Application Default Credentials name. On
// Cloud Run that is the server's own service account; on a developer machine
// it is `gcloud auth application-default login`. Nothing here creates or alters
// a schema: that is Migrate, run by the owner.
func NewCloudSQL(ctx context.Context, instance, database, user string) (*Postgres, error) {
	if instance == "" || database == "" || user == "" {
		return nil, fmt.Errorf("cloud sql: instance, database and user are all required")
	}
	dialer, err := cloudsqlconn.NewDialer(ctx, cloudsqlconn.WithIAMAuthN())
	if err != nil {
		return nil, fmt.Errorf("cloud sql connector: %w", err)
	}
	cfg, err := pgxpool.ParseConfig("")
	if err != nil {
		dialer.Close()
		return nil, err
	}
	cfg.ConnConfig.User = user
	cfg.ConnConfig.Database = database
	// The connector provides the encrypted channel and the IAM token, so pgx
	// asks for neither TLS nor a password of its own.
	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.Dial(ctx, instance)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		dialer.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		dialer.Close()
		return nil, fmt.Errorf("cloud sql %s: %w", instance, err)
	}
	return &Postgres{pool: pool, onClose: func() { dialer.Close() }}, nil
}

// CloudSQLUser is the IAM database user name for a service account email: Cloud
// SQL names it without the ".gserviceaccount.com" suffix. A person's Google
// account is used as is.
func CloudSQLUser(email string) string {
	return strings.TrimSuffix(email, ".gserviceaccount.com")
}
