package main

// `ob cred`: enroll, refresh and inspect credentials (PLAN.md, "Phase 5
// design"). Enroll and status run on the owner's machine with his own Google
// sign-in; refresh runs in the options-book-cred-refresher job. Nothing here ever
// prints a token or key: only version numbers and expiry.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/branonm/options-book/internal/cred"
)

// credVault lets tests swap Secret Manager for memory.
var credVault = func(ctx context.Context, project string) (cred.Vault, error) {
	return cred.NewSecretManager(ctx, project)
}

// credNow lets tests fix the clock.
var credNow = time.Now

// termUI is Enroll's UI on a terminal.
type termUI struct {
	out io.Writer
	in  *bufio.Reader
}

func (u termUI) Printf(format string, a ...any) { fmt.Fprintf(u.out, format, a...) }

func (u termUI) OpenURL(link string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", link).Start()
	case "linux":
		exec.Command("xdg-open", link).Start()
	}
}

func (u termUI) Line(prompt string) (string, error) {
	fmt.Fprint(u.out, prompt)
	s, err := u.in.ReadString('\n')
	return strings.TrimSpace(s), err
}

// Hidden reads without echo, from a terminal only: a secret never comes from
// argv, and a pipe would put it in shell history or a file.
func (u termUI) Hidden(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("this prompt needs a terminal; run `ob cred enroll` interactively")
	}
	fmt.Fprint(u.out, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(u.out)
	return string(b), err
}

func runCred(args []string) (int, error) {
	if len(args) == 0 {
		usage()
	}
	sub := args[0]
	fs := flag.NewFlagSet("cred "+sub, flag.ExitOnError)
	project := fs.String("project", os.Getenv("WHEEL_PROJECT"), "GCP project holding the secrets (or WHEEL_PROJECT)")
	force := fs.Bool("force", false, "refresh: refresh even with more than two days left")
	var rest []string
	if sub == "enroll" || sub == "refresh" {
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return 0, fmt.Errorf("ob cred %s: name the credential (%s)", sub, strings.Join(cred.IDs(), ", "))
		}
		rest = args[1:2]
		fs.Parse(args[2:])
	} else {
		fs.Parse(args[1:])
	}
	if *project == "" {
		return 0, fmt.Errorf("ob cred %s: --project is required", sub)
	}
	ctx := context.Background()
	v, err := credVault(ctx, *project)
	if err != nil {
		return 0, err
	}
	providers := cred.Providers(nil, credNow)

	switch sub {
	case "enroll":
		p, ok := providers[rest[0]]
		if !ok {
			return 0, fmt.Errorf("no credential %q (known: %s)", rest[0], strings.Join(cred.IDs(), ", "))
		}
		s, err := p.Enroll(ctx, termUI{out: stdout, in: bufio.NewReader(os.Stdin)})
		if err != nil {
			return 0, err
		}
		n, err := cred.Store(ctx, v, p, s)
		if err != nil {
			return 0, fmt.Errorf("enrolled, but storing it failed: %w", err)
		}
		st, err := cred.Describe(ctx, v, p, credNow())
		if err != nil {
			return 0, err
		}
		fmt.Fprintf(stdout, "Stored as version %d.\n%s\n", n, st)
		return 0, nil

	case "refresh":
		p, ok := providers[rest[0]]
		if !ok {
			return 0, fmt.Errorf("no credential %q (known: %s)", rest[0], strings.Join(cred.IDs(), ", "))
		}
		r, err := cred.Rotate(ctx, v, p, credNow(), *force)
		if err != nil {
			return 1, err
		}
		name, _ := cred.SecretName(p.ID())
		if !r.Refreshed {
			fmt.Fprintf(stdout, "%s version %d expires %s (%.1f days left); more than %s, so not refreshed.\n",
				name, r.Version, r.Expires.UTC().Format(time.RFC3339), r.Expires.Sub(credNow()).Hours()/24, cred.RefreshWindow)
			return 0, nil
		}
		fmt.Fprintf(stdout, "Refreshed %s: version %d, expires %s (%.1f days left). Disabled %v, destroyed %v.\n",
			name, r.Version, r.Expires.UTC().Format(time.RFC3339), r.Expires.Sub(credNow()).Hours()/24, r.Disabled, r.Destroyed)
		return 0, nil

	case "status":
		code := 0
		for _, id := range cred.IDs() {
			st, err := cred.Describe(ctx, v, providers[id], credNow())
			switch {
			case cred.IsNotExist(err):
				fmt.Fprintf(stdout, "%s: not enrolled (ob cred enroll %s)\n", id, id)
			case err != nil:
				fmt.Fprintf(stdout, "%s: %v\n", id, err)
				code = 1
			default:
				fmt.Fprintln(stdout, st)
				if st.DaysLeft != nil && *st.DaysLeft <= 0 {
					code = 1
				}
			}
		}
		return code, nil
	}
	usage()
	return 0, nil
}
