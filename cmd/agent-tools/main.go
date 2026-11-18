// Command agent-tools serves the agent's tools over MCP stdio: Alpaca
// paper-trading (positions, option chain, equity quotes, order staging),
// the Rule 6 risk check for proposals, and the rulebook citations straight from
// rules.json. It is a separate binary from `wheel`: `wheel` never contacts
// a broker, and the Alpaca tools here do, against the paper account only.
// The risk-check and the rulebook tools contact no broker.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/branonm/options-book/internal/layout"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/branonm/options-book/internal/alpaca"
	"github.com/branonm/options-book/internal/risk"
	"github.com/branonm/options-book/internal/rules"
)

func main() {
	book := flag.String("book", layout.DefaultBook, "the book whose rules to use: books/<book>/rules.json")
	rulesFlag := flag.String("rules", "", "a rules.json to use instead of the book's own")
	flag.Parse()
	rulesPath := rulesFlag
	if *rulesFlag == "" {
		p := filepath.FromSlash(layout.RulesFile(*book))
		rulesPath = &p
	}

	r, err := rules.Load(*rulesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	rulesDoc, err := rules.LoadRaw(*rulesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	cfg, err := alpaca.ConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
	client, err := alpaca.NewClient(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "wheel-agent-tools", Version: "v0.1.0"}, nil)
	alpaca.RegisterTools(server, client)
	risk.RegisterTools(server, r)
	rules.RegisterTools(server, rulesDoc)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}
