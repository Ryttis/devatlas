// Package cli implements the devatlas command-line interface.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Ryttis/devatlas/internal/httpserver"
	"github.com/Ryttis/devatlas/internal/version"
)

// Exit codes returned by Run.
const (
	ExitOK      = 0
	ExitFailure = 1 // runtime failure, e.g. the port is occupied
	ExitUsage   = 2 // invalid command line
)

const usageText = `DevAtlas — local project registry.

Usage:
  devatlas <command> [flags] [arguments]

Commands:
  add       Register a directory as a checkout of a project.
  list      List registered projects and their checkouts.
  remove    Remove a checkout registration by ID (never touches files).
  serve     Start the local HTTP server (loopback only).
  version   Print build information.
  help      Show this help.

Registrations are stored in a YAML file under the user configuration
directory (~/.config/devatlas/registrations.yaml). Override the location
with -config PATH on any command or with the DEVATLAS_CONFIG variable.

Run "devatlas <command> -h" for command flags.
`

// Run executes the CLI with the given arguments (without the program name)
// and returns the process exit code. Normal output goes to stdout; usage
// errors, logs, and failures go to stderr.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(context.Background(), args, stdout, stderr)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "error: missing command")
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		return runVersion(rest, stdout, stderr)
	case "serve":
		return runServe(ctx, rest, stderr)
	case "add":
		return runAdd(rest, stdout, stderr)
	case "list":
		return runList(rest, stdout, stderr)
	case "remove":
		return runRemove(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText)
		return ExitOK
	default:
		fmt.Fprintf(stderr, "error: unknown command %q\n", cmd)
		fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devatlas version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: devatlas version")
		fmt.Fprintln(stderr, "Print the version, commit, and Go runtime of this binary.")
	}
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return ExitUsage
	}
	fmt.Fprintln(stdout, version.Current())
	return ExitOK
}

func runServe(ctx context.Context, args []string, stderr io.Writer) int {
	cfg := httpserver.DefaultConfig()

	fs := flag.NewFlagSet("devatlas serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&cfg.Host, "host", cfg.Host, "loopback IP address to bind (127.0.0.1 or ::1)")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "TCP port to bind (0 picks a free port)")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "time allowed for in-flight requests on shutdown")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	configFlag := addConfigFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: devatlas serve [flags]")
		fmt.Fprintln(stderr, "Start the DevAtlas HTTP server. It only ever binds to a loopback address.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "error: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return ExitUsage
	}

	level, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}

	store, err := openStore(*configFlag, stderr)
	if err != nil {
		return ExitFailure
	}
	// Fail early on a malformed registration file instead of serving errors.
	if _, err := store.Load(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitFailure
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("using registration file", "path", store.Path())
	logger.Info("git observations are collected per request by running git locally; nothing is cached")

	srv, err := httpserver.New(cfg, store, logger)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}
	ln, err := srv.Listen()
	if err != nil {
		var inUse *httpserver.AddrInUseError
		if errors.As(err, &inUse) {
			logger.Error("cannot start server: port conflict", "addr", inUse.Addr, "cause", inUse.Err)
		}
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitFailure
	}

	// Stop on SIGINT (Ctrl+C) or SIGTERM. The signal handler is unregistered
	// as soon as the first signal arrives, so a second signal during a slow
	// graceful shutdown reaches the default handler and kills the process.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	if err := srv.Serve(ctx, ln); err != nil {
		logger.Error("server stopped with error", "err", err)
		return ExitFailure
	}
	return ExitOK
}

// usageError maps a flag parsing result to an exit code. The flag package has
// already printed the error and usage text to stderr.
// slogTo returns a warn-level text logger for command-line use.
func slogTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func usageError(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return ExitOK
	}
	return ExitUsage
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (use debug, info, warn, or error)", s)
}
