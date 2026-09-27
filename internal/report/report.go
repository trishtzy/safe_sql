// Package report renders lint results as human text, JSON, or GitHub
// workflow annotations.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/rules"
)

// Format names an output format.
type Format string

const (
	FormatHuman  Format = "human"
	FormatJSON   Format = "json"
	FormatGitHub Format = "github"
)

// Formats lists supported formats.
var Formats = []Format{FormatHuman, FormatJSON, FormatGitHub}

// Write renders res to w in the given format.
func Write(w io.Writer, res *lint.Result, format Format) error {
	switch format {
	case FormatJSON:
		return writeJSON(w, res)
	case FormatGitHub:
		return writeGitHub(w, res)
	case FormatHuman, "":
		return writeHuman(w, res)
	}
	return fmt.Errorf("unknown format %q", format)
}

func writeHuman(w io.Writer, res *lint.Result) error {
	for _, fl := range res.Failures {
		if fl.Line > 0 {
			fmt.Fprintf(w, "%s:%d: parse error: %s\n\n", fl.File, fl.Line, fl.Message)
		} else {
			fmt.Fprintf(w, "%s: parse error: %s\n\n", fl.File, fl.Message)
		}
	}
	for _, f := range res.Findings {
		fmt.Fprintf(w, "%s:%d: %s: %s\n", f.File, f.Line, f.Severity, f.RuleID)
		fmt.Fprintf(w, "  %s\n", indent(firstLines(f.Statement.Raw, 6), "  "))
		fmt.Fprintf(w, "\n  === %s ===\n\n", header(f.Severity))
		fmt.Fprintf(w, "  %s\n\n", f.Message)
		fmt.Fprintf(w, "  %s\n\n", indent(strings.TrimSpace(f.Guidance), "  "))
		fmt.Fprintf(w, "  To skip this check once it is safe, add the line\n    -- safe_sql:disable %s\n  directly above the statement.\n\n", f.RuleID)
	}
	if len(res.Findings) == 0 && len(res.Failures) == 0 {
		fmt.Fprintf(w, "No unsafe operations found in %d file(s).%s\n", len(res.Files), suppressedNote(res))
		return nil
	}
	fmt.Fprintln(w, res.Summary()+suppressedNote(res))
	return nil
}

// suppressedNote mentions findings hidden by the todo file, if any.
func suppressedNote(res *lint.Result) string {
	if res.Suppressed == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d known finding(s) suppressed by the todo file)", res.Suppressed)
}

func header(s rules.Severity) string {
	if s == rules.SeverityError {
		return "Dangerous operation detected #safe_sql"
	}
	return "Potentially unsafe operation #safe_sql"
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		return strings.Join(lines[:n], "\n") + "\n..."
	}
	return s
}

func indent(s, pad string) string {
	return strings.ReplaceAll(s, "\n", "\n"+pad)
}

type jsonFinding struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Guidance  string `json:"guidance"`
	Statement string `json:"statement"`
	Down      bool   `json:"down,omitempty"`
}

type jsonOut struct {
	Findings []jsonFinding  `json:"findings"`
	Failures []lint.Failure `json:"parse_failures"`
	Summary  struct {
		Errors     int `json:"errors"`
		Warnings   int `json:"warnings"`
		Files      int `json:"files"`
		Suppressed int `json:"suppressed,omitempty"`
	} `json:"summary"`
}

func writeJSON(w io.Writer, res *lint.Result) error {
	out := jsonOut{Findings: []jsonFinding{}, Failures: res.Failures}
	if out.Failures == nil {
		out.Failures = []lint.Failure{}
	}
	for _, f := range res.Findings {
		out.Findings = append(out.Findings, jsonFinding{
			File: f.File, Line: f.Line, Rule: f.RuleID, Severity: string(f.Severity),
			Message: f.Message, Guidance: f.Guidance, Statement: f.Statement.Raw, Down: f.Down,
		})
	}
	out.Summary.Errors, out.Summary.Warnings, out.Summary.Files = res.Errors(), res.Warnings(), len(res.Files)
	out.Summary.Suppressed = res.Suppressed
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func writeGitHub(w io.Writer, res *lint.Result) error {
	for _, fl := range res.Failures {
		fmt.Fprintf(w, "::error file=%s,line=%d,title=parse error::%s\n", fl.File, max(fl.Line, 1), escape(fl.Message))
	}
	for _, f := range res.Findings {
		level := "error"
		if f.Severity == rules.SeverityWarning {
			level = "warning"
		}
		fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s%%0A%%0A%s\n", level, f.File, f.Line, f.RuleID, escape(f.Message), escape(f.Guidance))
	}
	return nil
}

// escape encodes a message for a GitHub workflow command.
func escape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}
