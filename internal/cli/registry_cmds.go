package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Ryttis/devatlas/internal/httpserver"
	"github.com/Ryttis/devatlas/internal/registry"
	"github.com/Ryttis/devatlas/internal/registryfile"
)

// addConfigFlag registers the shared -config flag.
func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "registration file path (default: $"+registryfile.EnvConfigPath+" or the user configuration directory)")
}

// openStore resolves the registration file path and returns a store for it.
// Errors are printed to stderr.
func openStore(flagValue string, stderr io.Writer) (*registryfile.Store, error) {
	path, err := registryfile.ResolvePath(flagValue)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, err
	}
	return registryfile.NewStore(path), nil
}

func runAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devatlas add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	projectID := fs.String("project", "", "project ID (required): lowercase letters, digits, hyphens")
	name := fs.String("name", "", "project display name (required for a new project; must match for an existing one)")
	configFlag := addConfigFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: devatlas add -project ID [-name NAME] [-config PATH] DIRECTORY")
		fmt.Fprintln(stderr, "Register DIRECTORY as a checkout of project ID. The directory must exist;")
		fmt.Fprintln(stderr, "it is stored by its canonical absolute path and is never modified.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "error: expected exactly one DIRECTORY argument, got %d\n", fs.NArg())
		fs.Usage()
		return ExitUsage
	}
	if *projectID == "" {
		fmt.Fprintln(stderr, "error: -project is required")
		return ExitUsage
	}
	if err := registry.ValidateProjectID(*projectID); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitUsage
	}
	if *name != "" {
		if err := registry.ValidateProjectName(*name); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitUsage
		}
	}
	store, err := openStore(*configFlag, stderr)
	if err != nil {
		return ExitFailure
	}

	var res registry.AddResult
	err = store.Update(func(reg *registry.Registry) error {
		var err error
		res, err = reg.Add(registry.AddRequest{ProjectID: *projectID, Name: *name, Path: fs.Arg(0), Now: time.Now()})
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		if errors.Is(err, registry.ErrInvalidPath) || errors.Is(err, registry.ErrProjectNameRequired) {
			return ExitUsage
		}
		return ExitFailure
	}
	if res.ProjectCreated {
		fmt.Fprintf(stdout, "created project %s (%s)\n", res.Project.ID, res.Project.Name)
	}
	fmt.Fprintf(stdout, "registered checkout %s: %s -> project %s\n", res.Checkout.ID, res.Checkout.Path, res.Project.ID)
	fmt.Fprintf(stdout, "registration file: %s\n", store.Path())
	return ExitOK
}

func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devatlas list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the same JSON document as GET /api/projects")
	configFlag := addConfigFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: devatlas list [-json] [-config PATH]")
		fmt.Fprintln(stderr, "List registered projects and their checkouts.")
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
	store, err := openStore(*configFlag, stderr)
	if err != nil {
		return ExitFailure
	}
	reg, err := store.Load()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitFailure
	}
	resp := httpserver.BuildProjectsResponse(reg.Views(), store.Path(), time.Now())

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return ExitFailure
		}
		return ExitOK
	}

	fmt.Fprintf(stdout, "registration file: %s\n", store.Path())
	if len(resp.Projects) == 0 {
		fmt.Fprintln(stdout, "no projects registered")
		return ExitOK
	}
	fmt.Fprintln(stdout, "runtime status: not collected in this build (every checkout is \"unknown\")")
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tNAME\tCHECKOUT\tPATH\tDIRECTORY")
	for _, p := range resp.Projects {
		for _, c := range p.Checkouts {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Name, c.ID, c.Path, c.PathStatus)
		}
	}
	tw.Flush()
	return ExitOK
}

func runRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devatlas remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFlag := addConfigFlag(fs)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: devatlas remove [-config PATH] CHECKOUT_ID")
		fmt.Fprintln(stderr, "Remove a checkout registration. Only the registry changes; the directory,")
		fmt.Fprintln(stderr, "its files, and any containers are left alone. Removing the last checkout")
		fmt.Fprintln(stderr, "of a project also removes the project record.")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "error: expected exactly one CHECKOUT_ID argument, got %d\n", fs.NArg())
		fs.Usage()
		return ExitUsage
	}
	store, err := openStore(*configFlag, stderr)
	if err != nil {
		return ExitFailure
	}
	var res registry.RemoveResult
	err = store.Update(func(reg *registry.Registry) error {
		var err error
		res, err = reg.Remove(fs.Arg(0))
		return err
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return ExitFailure
	}
	fmt.Fprintf(stdout, "removed checkout %s (%s) from project %s; files were not touched\n", res.Checkout.ID, res.Checkout.Path, res.Checkout.ProjectID)
	if res.ProjectRemoved {
		fmt.Fprintf(stdout, "removed project %s (%s): it had no other checkouts\n", res.Project.ID, res.Project.Name)
	}
	return ExitOK
}
