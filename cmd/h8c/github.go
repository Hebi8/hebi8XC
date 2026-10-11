package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Hebi8/hebi8XC/internal/control"
	"github.com/Hebi8/hebi8XC/internal/github"
	"github.com/Hebi8/hebi8XC/internal/identity"
	"github.com/Hebi8/hebi8XC/internal/node"
	"github.com/Hebi8/hebi8XC/internal/roster"
	"github.com/Hebi8/hebi8XC/internal/sshkey"
)

// GitHub as the commands reach it. Tests point these at a fake.
var (
	gh       = github.New()
	pollWait = github.Sleep
)

const keySettingsURL = "https://github.com/settings/keys"

// login runs the device flow, registers this node's key as an SSH signing
// key of the account and records which account that is. The token only
// lives in this function: reading the roster later needs none.
func login(dir string, out io.Writer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ident, err := identity.Load(dir)
	if err != nil {
		return err
	}
	code, err := gh.RequestDeviceCode(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Open %s and enter the code %s\nWaiting for GitHub...\n", code.VerificationURI, code.UserCode)
	token, err := gh.WaitForToken(ctx, code, pollWait)
	if err != nil {
		return err
	}
	account, err := gh.Viewer(ctx, token)
	if err != nil {
		return err
	}

	registered, err := gh.ViewerSigningKeys(ctx, token)
	if err != nil {
		return err
	}
	found, title := false, ""
	for _, k := range registered {
		if key, _, err := sshkey.Parse(k.Key); err == nil && key.Equal(ident.Public()) {
			found, title = true, k.Title
		}
	}
	switch {
	case !found:
		title = roster.KeyTitle(ident.Name, ident.ID)
		if err := gh.AddSigningKey(ctx, token, title, sshkey.Marshal(ident.Public())); err != nil {
			return fmt.Errorf("registering this node's key: %w", err)
		}
		fmt.Fprintf(out, "Registered this node's key on the account as %q.\n", title)
	case strings.HasPrefix(title, roster.KeyTitlePrefix):
		fmt.Fprintf(out, "This node's key is already on the account as %q.\n", title)
	default:
		return fmt.Errorf("this node's key is on the account as %q, without the %q that marks a node key; delete it at %s and run h8c login again",
			title, roster.KeyTitlePrefix, keySettingsURL)
	}

	if err := github.SaveAccount(filepath.Join(dir, node.AccountFile), account); err != nil {
		return err
	}
	fmt.Fprintf(out, "Logged in as %s (id %d).\n", account.Login, account.ID)

	// Fill the roster cache the way a node does, without the token.
	remote, err := openGitHubRoster(dir, account, func(warning error) { fmt.Fprintln(out, "warning:", warning) })
	if err != nil {
		return err
	}
	if err := remote.Refresh(ctx, account.User()); err != nil {
		fmt.Fprintln(out, "warning:", err)
	} else {
		fmt.Fprintf(out, "The account lists %d node keys.\n", len(remote.Entries(account.User())))
	}

	var running node.Status
	err = control.Dial(filepath.Join(dir, node.ControlSocket)).Get(ctx, "/v1/status", &running)
	if err == nil && running.User != account.User() {
		fmt.Fprintln(out, "A node is running here without this login; restart h8c up to use it.")
	}
	return nil
}

// openGitHubRoster opens the roster cache of a logged-in data directory. A
// cache file that cannot be used is reported through warn and otherwise
// treated as missing; the next refresh replaces it.
func openGitHubRoster(dir string, account github.Account, warn func(error)) (*roster.GitHub, error) {
	remote, err := roster.OpenGitHub(filepath.Join(dir, node.GitHubRosterFile), gh, account.Login, time.Now)
	if err != nil {
		return nil, err
	}
	if err := remote.Discarded(); err != nil {
		warn(err)
	}
	return remote, nil
}

// accountLine describes the GitHub login of a data directory for h8c id.
func accountLine(dir string) (string, error) {
	account, loggedIn, err := github.LoadAccount(filepath.Join(dir, node.AccountFile))
	if err != nil {
		return "", err
	}
	if !loggedIn {
		return "not logged in", nil
	}
	return fmt.Sprintf("%s (id %d)", account.Login, account.ID), nil
}

func showRoster(dir string, args []string, out io.Writer) error {
	account, loggedIn, err := github.LoadAccount(filepath.Join(dir, node.AccountFile))
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if args[0] != "refresh" || len(args) > 1 {
			return errors.New("want: h8c roster [refresh]")
		}
		if !loggedIn {
			return errors.New("not logged in to GitHub; run h8c login")
		}
		// A running node refreshes its own roster and answers when it is
		// done. Without one, the cache file is refreshed for the next start.
		err := control.Dial(filepath.Join(dir, node.ControlSocket)).Post(context.Background(), "/v1/roster/refresh")
		if errors.Is(err, control.ErrNotRunning) {
			remote, openErr := openGitHubRoster(dir, account, func(error) {})
			if openErr != nil {
				return openErr
			}
			err = remote.Refresh(context.Background(), account.User())
		}
		if err != nil {
			return err
		}
	}

	local, err := roster.OpenFile(filepath.Join(dir, node.RosterFile))
	if err != nil {
		return err
	}
	var remote []roster.Entry
	if !loggedIn {
		fmt.Fprintln(out, "github  not logged in")
	} else {
		cache, err := openGitHubRoster(dir, account, func(warning error) { fmt.Fprintln(out, "warning:", warning) })
		if err != nil {
			return err
		}
		remote = cache.Entries(account.User())
		login, fetched := cache.Fetched(account.User())
		if fetched.IsZero() {
			fmt.Fprintf(out, "github  %s (id %d), never fetched\n", account.Login, account.ID)
		} else {
			fmt.Fprintf(out, "github  %s (id %d), %d node keys, fetched %s\n",
				login, account.ID, len(remote), fetched.Local().Format(time.DateTime))
		}
	}
	fmt.Fprintf(out, "local   %d keys added by hand\n\n", len(local.Entries()))

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tKEY\tNAME")
	for _, e := range remote {
		fmt.Fprintf(tw, "github\t%s\t%s\n", sshkey.Marshal(e.Key), e.Comment)
	}
	for _, e := range local.Entries() {
		fmt.Fprintf(tw, "local\t%s\t%s\n", sshkey.Marshal(e.Key), e.Comment)
	}
	return tw.Flush()
}
