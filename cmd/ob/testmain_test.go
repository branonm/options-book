package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/branonm/options-book/internal/store/storetest"
)

// repoDir is the repo root the tests read: the synthetic one in
// testdata/synthetic. The tests
// in this package name snapshots, history and fixtures relative to it, so
// TestMain moves there before any test runs. moduleDir is the code tree
// (where go.mod is), for the fixtures that live beside the code.
var repoDir, moduleDir string

func TestMain(m *testing.M) {
	d, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			break
		}
		up := filepath.Dir(d)
		if up == d {
			fmt.Fprintln(os.Stderr, "no go.mod above the test directory")
			os.Exit(1)
		}
		d = up
	}
	moduleDir = d
	paperRoot = filepath.Join(d, "internal", "report", "testdata", "paper")
	alpacaSamps = filepath.Join(d, "internal", "broker", "alpaca", "testdata")
	repoDir = storetest.DataRoot(d)
	if v := os.Getenv("OB_TEST_ROOT"); v != "" {
		repoDir = v
	}
	if err := os.Chdir(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
