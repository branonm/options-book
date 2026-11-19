// Command ob-mcp is the remote MCP server in the storage design: the
// wheel-book tools over the Firestore store, behind Google sign-in, on Cloud
// Run. It serves the read and write tools (internal/obmcp) and, since phase
// 5, fetch_snapshot, which pulls from Robinhood through internal/rhmcp's
// read-only allowlist using the token it reads from Secret Manager (it never
// writes or refreshes a credential). It has no order path, and every store
// write is a create.
//
// accounts.json and every book (books/<book>/{book,rules}.json) are compiled in
// (config/embed.go, books/embed.go), so a rule
// change is a commit and a deploy, and every run records their sha256.
//
// Configuration (environment):
//
//	OAUTH_CLIENT_ID          Google OAuth client id Claude signs in with (required)
//	ALLOWED_EMAIL            the one Google account allowed in (required)
//	PUBLIC_URL               https://<service>.run.app, no trailing slash (required)
//	PROJECT_ID               GCP project holding the Firestore database (required)
//	FIRESTORE_DB             database id (default "(default)")
//	STORE                    "firestore" (default), "cloudsql" or "postgres" (local testing only)
//	CLOUDSQL_INSTANCE        STORE=cloudsql: the connection name, PROJECT:REGION:INSTANCE
//	CLOUDSQL_DATABASE        STORE=cloudsql: the database name
//	CLOUDSQL_USER            STORE=cloudsql: the IAM database user (the service account email without .gserviceaccount.com)
//	POSTGRES_DSN             STORE=postgres: a DSN for a local database (local testing only)
//	CACHE_TTL                how long an opened, verified store is reused (default 5m)
//	FIRESTORE_EMULATOR_HOST  use the local emulator instead (local testing only)
//	LOCAL_TESTING            "true" allows an http PUBLIC_URL (local testing only)
//	PORT                     listen port (default 8080; Cloud Run sets it)
package main

import (
	"context"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/books"
	configfiles "github.com/branonm/options-book/config"
	"github.com/branonm/options-book/internal/config"
	"github.com/branonm/options-book/internal/cred"
	"github.com/branonm/options-book/internal/gauth"
	"github.com/branonm/options-book/internal/rules"
	"github.com/branonm/options-book/internal/store"
	"github.com/branonm/options-book/internal/obmcp"
)

// commit is set at build time: -ldflags "-X main.commit=<sha>".
var commit = ""

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	get := os.Getenv
	auth := gauth.Config{
		ClientID:     get("OAUTH_CLIENT_ID"),
		AllowedEmail: get("ALLOWED_EMAIL"),
		PublicURL:    trimSlash(get("PUBLIC_URL")),
		ResourceName: "options-book",
		LocalTesting: get("LOCAL_TESTING") == "true",
	}
	if err := auth.Validate(); err != nil {
		return err
	}
	project, database := get("PROJECT_ID"), get("FIRESTORE_DB")
	if project == "" {
		return fmt.Errorf("missing environment: PROJECT_ID")
	}
	if database == "" {
		database = "(default)"
	}
	ttl := 5 * time.Minute
	if v := get("CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("CACHE_TTL: %w", err)
		}
		ttl = d
	}

	deps, err := newDepsStore(project, database, get("FIRESTORE_EMULATOR_HOST"), storeConfig{
		Kind: get("STORE"), Instance: get("CLOUDSQL_INSTANCE"), Database: get("CLOUDSQL_DATABASE"), User: get("CLOUDSQL_USER"), DSN: get("POSTGRES_DSN"),
	}, ttl)
	if err != nil {
		return err
	}
	handler := newHandler(auth, gauth.NewGoogleVerifier(auth.ClientID, auth.AllowedEmail), deps)

	port := get("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("ob-mcp %s listening on :%s, resource %s, store %s", deps.Commit, port, auth.Resource(), deps.Spec)
	return http.ListenAndServe(":"+port, handler)
}

// newHandler is the whole server: sign-in metadata, the token check, and the
// tools over stateless streamable HTTP.
func newHandler(auth gauth.Config, v gauth.Verifier, deps *obmcp.Deps) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "options-book", Version: deps.Commit}, &mcp.ServerOptions{Instructions: obmcp.Instructions})
	obmcp.Register(server, deps)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return gauth.Handler(auth, v, mcpHandler)
}

// storeConfig picks the store behind the server. The zero value is Firestore,
// which is what is deployed today; Cloud SQL is chosen by STORE=cloudsql.
type storeConfig struct {
	Kind     string // "", "firestore", "cloudsql" or "postgres"
	Instance string // cloudsql: PROJECT:REGION:INSTANCE
	Database string // cloudsql: database name
	User     string // cloudsql: IAM database user
	DSN      string // postgres: local testing only
}

func newDeps(project, database, emulator string, ttl time.Duration) (*obmcp.Deps, error) {
	return newDepsStore(project, database, emulator, storeConfig{}, ttl)
}

func newDepsStore(project, database, emulator string, sc storeConfig, ttl time.Duration) (*obmcp.Deps, error) {
	accounts, err := config.ParseAccounts(layout.AccountsConfig+" (compiled in)", configfiles.Accounts)
	if err != nil {
		return nil, err
	}
	bks := map[string]*obmcp.BookConfig{}
	for _, name := range books.Names() {
		bb, err := books.Book(name)
		if err != nil {
			return nil, err
		}
		rb, err := books.Rules(name)
		if err != nil {
			return nil, err
		}
		cfg, err := config.ParseBook(name, layout.BookFile(name)+" (compiled in)", bb, accounts)
		if err != nil {
			return nil, err
		}
		r, err := rules.Parse(layout.RulesFile(name)+" (compiled in)", rb)
		if err != nil {
			return nil, err
		}
		bks[name] = &obmcp.BookConfig{Name: name, Cfg: cfg, Rules: r, BookSHA: store.SHA256(bb), RulesSHA: store.SHA256(rb)}
	}
	if bks[layout.DefaultBook] == nil {
		return nil, fmt.Errorf("the default book %q is not compiled in (have %v)", layout.DefaultBook, books.Names())
	}
	var db store.DB
	spec := "firestore=" + project
	switch {
	case emulator != "":
		db, spec = store.NewEmulator(emulator, project), "emulator="+emulator
	case sc.Kind == "cloudsql":
		pg, err := store.NewCloudSQL(context.Background(), sc.Instance, sc.Database, sc.User)
		if err != nil {
			return nil, err
		}
		db, spec = pg, "cloudsql="+sc.Instance+"/"+sc.Database
	case sc.Kind == "postgres":
		if sc.DSN == "" {
			return nil, fmt.Errorf("STORE=postgres needs POSTGRES_DSN")
		}
		pg, err := store.NewPostgres(context.Background(), sc.DSN, "")
		if err != nil {
			return nil, err
		}
		db, spec = pg, "postgres"
	case sc.Kind == "" || sc.Kind == "firestore":
		fdb, err := store.NewFirestore(context.Background(), project, database)
		if err != nil {
			return nil, err
		}
		db = fdb
	default:
		return nil, fmt.Errorf("STORE=%q: want firestore, cloudsql or postgres", sc.Kind)
	}
	d := &obmcp.Deps{
		Books: bks, DefaultBook: layout.DefaultBook, Accounts: accounts, AccountsSHA: store.SHA256(configfiles.Accounts),
		DB: db, Spec: spec, Commit: buildCommit(), CacheTTL: ttl,
	}
	// Phase 5: on the live store, the credentials are read from Secret Manager
	// (the server's service account can read the latest versions, never write).
	// The emulator has no credentials, so fetch_snapshot and cred_status aren't offered.
	if emulator == "" && sc.Kind != "postgres" {
		v, err := cred.NewSecretManager(context.Background(), project)
		if err != nil {
			return nil, err
		}
		d.Vault = v
		d.Robinhood = cred.NewRobinhood(nil, nil)
		d.AlpacaPaper = cred.NewAlpacaPaper(nil)
		d.RobinhoodMCP = cred.RobinhoodMCP
		d.RobinhoodHTTP = &http.Client{Timeout: 60 * time.Second}
	}
	return d, nil
}

// buildCommit is the -X value, else the commit Go stamped into the binary.
func buildCommit() string {
	if commit != "" {
		return commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" && dirty {
			return rev + "-dirty"
		}
		if rev != "" {
			return rev
		}
	}
	return "unknown"
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
