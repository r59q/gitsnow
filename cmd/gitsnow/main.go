package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/r59q/gitsnow/internal/issues"
	"github.com/r59q/gitsnow/internal/server"
	"github.com/r59q/gitsnow/internal/web"
)

const usage = `gitsnow: local-first repository management

Usage:
  gitsnow serve [--repo PATH] [--listen 127.0.0.1:7331] [--sync-interval 15s]
  gitsnow sync [--repo PATH] [--remote NAME|off]
  gitsnow issue list [--repo PATH] [--status open|closed] [--search TEXT]
  gitsnow issue create [--repo PATH] --title TITLE [--body TEXT] [--id ID]
  gitsnow issue show [--repo PATH] ID
  gitsnow issue update [--repo PATH] [--title TITLE] [--body TEXT] [--status open|closed] [--revision REV] ID
  gitsnow issue delete [--repo PATH] --revision REV ID

Issue commands return JSON and operate directly on Git; no daemon is required.
Place flags before the issue ID. Metadata lives on the gitsnow/meta branch.
Metadata is shared automatically through origin when it exists. Select a
different remote or disable sharing with gitsnow sync --remote NAME|off.
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gitsnow:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, errorOutput io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		_, err := io.WriteString(output, usage)
		return err
	}
	if args[0] == "serve" {
		return serve(ctx, args[1:], errorOutput)
	}
	if args[0] == "sync" {
		return syncMetadata(ctx, args[1:], output, errorOutput)
	}
	if args[0] != "issue" || len(args) < 2 {
		return fmt.Errorf("unknown command; run gitsnow --help")
	}
	command := args[1]
	flags := flag.NewFlagSet("issue "+command, flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	repo := flags.String("repo", ".", "Git working repository")
	var title, body, status, query, id, revision *string
	switch command {
	case "list":
		status = flags.String("status", "", "Filter by open or closed")
		query = flags.String("search", "", "Search title and body")
	case "create":
		title = flags.String("title", "", "Issue title")
		body = flags.String("body", "", "Markdown issue body")
		id = flags.String("id", "", "Optional stable ID for idempotent creation")
	case "show":
	case "update":
		title = flags.String("title", "", "New issue title")
		body = flags.String("body", "", "New Markdown body")
		status = flags.String("status", "", "New status")
		revision = flags.String("revision", "", "Expected issue revision; defaults to revision read at command start")
	case "delete":
		revision = flags.String("revision", "", "Required expected issue revision")
	default:
		return fmt.Errorf("unknown issue command %q", command)
	}
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	expectedArgs := 0
	if command == "show" || command == "update" || command == "delete" {
		expectedArgs = 1
	}
	if flags.NArg() != expectedArgs {
		return fmt.Errorf("%s expects %d positional argument(s); place flags before the issue ID", command, expectedArgs)
	}
	if command == "delete" && *revision == "" {
		return fmt.Errorf("delete requires --revision from issue show")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	store, err := issues.Open(ctx, *repo)
	if err != nil {
		return err
	}
	defer store.Close()
	if command == "list" || command == "show" {
		if _, err := store.Sync(ctx); err != nil {
			fmt.Fprintln(errorOutput, "gitsnow: could not fetch shared metadata; showing local records:", err)
		}
	}
	var result any
	switch command {
	case "list":
		result, err = store.List(ctx, *status, *query)
	case "create":
		result, err = store.Create(ctx, issues.Input{ID: *id, Title: *title, Body: *body})
	case "show":
		result, err = store.Get(ctx, flags.Arg(0))
	case "update":
		var current issues.Issue
		current, err = store.Get(ctx, flags.Arg(0))
		if err != nil {
			break
		}
		input := issues.Input{Title: current.Title, Body: current.Body, Status: current.Status, Revision: current.Revision}
		flags.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "title":
				input.Title = *title
			case "body":
				input.Body = *body
			case "status":
				input.Status = *status
			case "revision":
				input.Revision = *revision
			}
		})
		result, err = store.Update(ctx, current.ID, input)
	case "delete":
		err = store.Delete(ctx, flags.Arg(0), *revision)
		result = map[string]string{"deleted": flags.Arg(0)}
	}
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return err
	}
	if command == "create" || command == "update" || command == "delete" {
		status, err := store.Sync(ctx)
		if err != nil {
			return fmt.Errorf("issue change saved locally, but not shared (will retry while the daemon runs): %w", err)
		}
		fmt.Fprintln(errorOutput, "gitsnow:", status.Message)
	}
	return nil
}

func syncMetadata(ctx context.Context, args []string, output, errorOutput io.Writer) error {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	repo := flags.String("repo", ".", "Git working repository")
	remote := flags.String("remote", "", "Persist a synchronization remote name, or off for local-only mode")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("sync does not accept positional arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	configure := false
	flags.Visit(func(f *flag.Flag) { configure = configure || f.Name == "remote" })
	if configure {
		if err := issues.ConfigureSync(ctx, *repo, *remote); err != nil {
			return err
		}
	}
	store, err := issues.Open(ctx, *repo)
	if err != nil {
		return err
	}
	defer store.Close()
	status, syncErr := store.Sync(ctx)
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return errors.Join(syncErr, encoder.Encode(status))
}

func serve(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(output)
	repo := flags.String("repo", ".", "Git working repository")
	listen := flags.String("listen", "127.0.0.1:7331", "Loopback address for local UI and API")
	syncInterval := flags.Duration("sync-interval", 15*time.Second, "Automatic metadata fetch/retry interval (minimum 1s)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	if *syncInterval < time.Second {
		return fmt.Errorf("--sync-interval must be at least 1s")
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil || !server.LoopbackHost(host) {
		return fmt.Errorf("--listen must be a loopback host:port; remote hosting is not enabled")
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	store, err := issues.Open(startup, *repo)
	cancel()
	if err != nil {
		return err
	}
	defer store.Close()
	assets, err := fs.Sub(web.Assets, "dist")
	if err != nil {
		return fmt.Errorf("open embedded UI: %w", err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		listener.Close()
		return fmt.Errorf("resolved listener is not loopback")
	}
	logger := slog.New(slog.NewTextHandler(output, nil))
	syncContext, stopSync := context.WithCancel(ctx)
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		store.RunSync(syncContext, *syncInterval, func(err error) {
			logger.Error("metadata synchronization failed; local changes are preserved", "error", err)
		})
	}()
	defer func() {
		stopSync()
		<-syncDone
	}()
	httpServer := &http.Server{
		Handler: server.New(store, assets, logger), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 * 1024,
	}
	done := make(chan error, 1)
	go func() { done <- httpServer.Serve(listener) }()
	logger.Info("gitsnow is listening", "url", "http://"+listener.Addr().String(), "repository", store.Root())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			httpServer.Close()
			return fmt.Errorf("stop server: %w", err)
		}
		return nil
	}
}
