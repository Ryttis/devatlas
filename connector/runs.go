package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

var (
	runIDPattern = regexp.MustCompile(`^run-[A-Za-z0-9_-]{6,64}$`)
	shaPattern   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	errNoRun     = errors.New("run not found or its status is unavailable")
)

type runStatus struct {
	ID          string    `json:"run_id"`
	Stage       string    `json:"stage"`
	FailedStage string    `json:"failed_stage,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	Base        string    `json:"base"`
	Head        string    `json:"head,omitempty"`
	GoChecks    string    `json:"go_checks"`
	Review      string    `json:"review"`
}

type runList struct {
	Runs    []runStatus `json:"runs"`
	Skipped int         `json:"unavailable_records"`
}

type runStore struct{ dir string }

func readRun(root *os.Root, id string) (runStatus, error) {
	var out runStatus
	if !runIDPattern.MatchString(id) {
		return out, errNoRun
	}
	entry, err := root.Lstat(id)
	if err != nil || !entry.IsDir() {
		return out, errNoRun
	}
	name := filepath.Join(id, "state.json")
	entry, err = root.Lstat(name)
	if err != nil || !entry.Mode().IsRegular() {
		return out, errNoRun
	}
	f, err := root.Open(name)
	if err != nil {
		return out, errNoRun
	}
	defer f.Close()
	const maxState = 16 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxState+1))
	if err != nil || len(b) > maxState || json.Unmarshal(b, &out) != nil ||
		out.ID != id || out.UpdatedAt.IsZero() || !shaPattern.MatchString(out.Base) ||
		(out.Head != "" && !shaPattern.MatchString(out.Head)) {
		return runStatus{}, errNoRun
	}
	out.GoChecks, out.Review = "not_confirmed", "not_available"
	if out.Stage != "failed" {
		out.FailedStage = ""
	}
	switch out.Stage {
	case "preparing", "coding", "verifying":
	case "reviewing", "pass", "changes_requested":
		if out.Head == "" {
			return runStatus{}, errNoRun
		}
		out.GoChecks = "passed"
		if out.Stage != "reviewing" {
			out.Review = out.Stage
		}
	case "failed":
		switch out.FailedStage {
		case "preparing", "coding", "verifying":
		case "reviewing":
			if out.Head == "" {
				return runStatus{}, errNoRun
			}
			out.GoChecks = "passed"
		default:
			return runStatus{}, errNoRun
		}
	default:
		return runStatus{}, errNoRun
	}
	return out, nil
}

func (s runStore) get(id string) (runStatus, error) {
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return runStatus{}, errNoRun
	}
	defer root.Close()
	return readRun(root, id)
}

func (s runStore) list(limit int) (runList, error) {
	out := runList{Runs: []runStatus{}}
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 20 {
		return out, errors.New("limit must be between 1 and 20")
	}
	root, err := os.OpenRoot(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, errors.New("run history is unavailable")
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return out, errors.New("run history is unavailable")
	}
	defer dir.Close()
	const maxEntries = 2000
	entries, err := dir.ReadDir(maxEntries + 1)
	if (err != nil && !errors.Is(err, io.EOF)) || len(entries) > maxEntries {
		return out, errors.New("run history is unavailable or exceeds the scan limit")
	}
	for _, entry := range entries {
		if !runIDPattern.MatchString(entry.Name()) {
			continue
		}
		run, err := readRun(root, entry.Name())
		if err != nil {
			out.Skipped++
			continue
		}
		out.Runs = append(out.Runs, run)
	}
	slices.SortFunc(out.Runs, func(a, b runStatus) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	out.Runs = out.Runs[:min(limit, len(out.Runs))]
	return out, nil
}
