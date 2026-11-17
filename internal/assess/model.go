// Package assess turns collected facts and external data into a stored
// technology status per project: a freshness and a security status decided
// by documented rules, findings with evidence and sources, and optional AI
// explanations that can neither change a status nor add or remove findings.
package assess

import (
	"time"

	"github.com/Ryttis/devatlas/internal/collect/stack"
	"github.com/Ryttis/devatlas/internal/sources"
)

// Versions. RulesVersion changes whenever a rule or threshold changes;
// stored results from another version are shown as stale.
const (
	SchemaVersion = 1
	RulesVersion  = "2026-11-17.1"
)

// Statuses of an area.
const (
	StatusGreen      = "green"
	StatusYellow     = "yellow"
	StatusRed        = "red"
	StatusUnverified = "unverified"
)

// Finding levels. Info never affects a status.
const (
	LevelRed    = "red"
	LevelYellow = "yellow"
	LevelInfo   = "info"
)

// Areas.
const (
	AreaFreshness = "freshness"
	AreaSecurity  = "security"
)

// Record is the stored state of one project: the last successful result and
// the last attempt, which may be newer and may have failed.
type Record struct {
	SchemaVersion int     `json:"schema_version"`
	ProjectID     string  `json:"project_id"`
	Result        *Result `json:"result"`
	LastAttempt   Attempt `json:"last_attempt"`
}

// Attempt describes the most recent assessment run.
type Attempt struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	CheckoutID string    `json:"checkout_id"`
	// Error is display-safe text; empty when OK.
	Error string `json:"error,omitempty"`
}

// CheckoutRef ties a result to exactly what was assessed.
type CheckoutRef struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// GitStatus is the Git observation status; Head, Branch and Dirty are
	// set only when it is "ok".
	GitStatus string `json:"git_status"`
	Head      string `json:"head,omitempty"`
	Branch    string `json:"branch,omitempty"`
	// Dirty is nil when Git state is unknown.
	Dirty     *bool  `json:"dirty"`
	FilesHash string `json:"files_hash"`
}

// Result is one successful assessment.
type Result struct {
	RulesVersion string      `json:"rules_version"`
	Checkout     CheckoutRef `json:"checkout"`
	// SourcesCheckedAt is when the external sources were queried and the
	// rules evaluated. Support dates were compared with this date.
	SourcesCheckedAt time.Time         `json:"sources_checked_at"`
	Description      string            `json:"description,omitempty"`
	Files            []stack.FileRef   `json:"files"`
	Ecosystems       []stack.Ecosystem `json:"ecosystems"`
	Technologies     []TechView        `json:"technologies"`
	Packages         []PackageView     `json:"packages"`
	Freshness        Area              `json:"freshness"`
	Security         Area              `json:"security"`
	Findings         []Finding         `json:"findings"`
	Sources          []SourceRef       `json:"sources"`
	AI               AIResult          `json:"ai"`
	Requests         int               `json:"external_requests"`
}

// Area is the rule outcome for freshness or security.
type Area struct {
	Status string `json:"status"`
	// Partial is true when there are findings but the scope was not fully
	// checked: the colour stands, the coverage does not.
	Partial bool        `json:"partial"`
	Reason  string      `json:"reason"`
	Gaps    []stack.Gap `json:"gaps"`
	Checked string      `json:"checked"`
}

// TechView is a technology with its support evaluation.
type TechView struct {
	stack.Technology
	// Cycle is the matched release cycle; empty when none matched.
	Cycle      string `json:"cycle,omitempty"`
	EOLFrom    string `json:"eol_from,omitempty"`
	EOASFrom   string `json:"eoas_from,omitempty"`
	LatestInCy string `json:"latest_in_cycle,omitempty"`
	// Support is supported, eol, eol-soon, or unverified.
	Support string `json:"support"`
	Note    string `json:"note,omitempty"`
}

// PackageView is a package with what the external sources said about it.
type PackageView struct {
	stack.Package
	// Checked says whether the package was sent to OSV.
	Checked    bool               `json:"checked"`
	Unchecked  string             `json:"unchecked_reason,omitempty"`
	Latest     string             `json:"latest,omitempty"`
	Deprecated bool               `json:"deprecated,omitempty"`
	Advisories []sources.Advisory `json:"advisories,omitempty"`
}

// Finding is one problem established by a rule.
type Finding struct {
	ID    string `json:"id"`
	Area  string `json:"area"`
	Level string `json:"level"`
	// Rule is the identifier of the rule in the documented criteria.
	Rule     string     `json:"rule"`
	Title    string     `json:"title"`
	Subject  string     `json:"subject"`
	Evidence []Evidence `json:"evidence"`
	Sources  []Link     `json:"sources"`
	// Action is the rule's own recommended action.
	Action string `json:"action"`
}

// Evidence is one labelled fact supporting a finding.
type Evidence struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Link names a source. URLs are built by DevAtlas from fixed patterns,
// never copied from external or AI text.
type Link struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// SourceRef records that an external source was consulted.
type SourceRef struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Purpose string `json:"purpose"`
}

// AI statuses.
const (
	AIOK        = "ok"
	AIDisabled  = "disabled"
	AINotNeeded = "not_needed"
	AIFailed    = "failed"
	AIRejected  = "rejected"
)

// AIResult holds explanations attached to existing findings.
type AIResult struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	// GeneratedAt is when the texts were generated; it does not move when
	// they are reused.
	GeneratedAt time.Time `json:"generated_at,omitempty"`
	// Key identifies the input the texts were generated from.
	Key     string            `json:"key,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Reused  bool              `json:"reused,omitempty"`
	Texts   map[string]AIText `json:"texts,omitempty"`
	Dropped int               `json:"dropped,omitempty"`
}

// AIText is the explanation of one finding.
type AIText struct {
	Explanation string `json:"explanation"`
	Action      string `json:"action"`
}
