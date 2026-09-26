package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/fix"
	"github.com/trishtzy/safe_sql/internal/fix/ai"
	"github.com/trishtzy/safe_sql/internal/lint"
	"github.com/trishtzy/safe_sql/internal/migrate"
	"github.com/trishtzy/safe_sql/internal/report"
)

// fixOutput is the JSON shape of `safe_sql fix --format json`, consumed by
// the GitHub Action to deliver changes.
type fixOutput struct {
	Modified  []string          `json:"modified"`
	Created   []string          `json:"created"`
	Files     map[string]string `json:"files"`
	Diffs     map[string]string `json:"diffs"`
	Notes     []string          `json:"notes"`
	Summaries []string          `json:"summaries"`
	Declined  []string          `json:"declined"`
	Rejected  []string          `json:"rejected"`
	Model     string            `json:"model,omitempty"`
	Remaining int               `json:"remaining_findings"`
	Unfixed   []string          `json:"unfixed"`
}

func newFixCmd(code *int, configPath *string) *cobra.Command {
	var (
		fl            lintFlags
		useAI, dryRun bool
		model         string
		maxIter       int
		allowDisable  bool
	)
	cmd := &cobra.Command{
		Use:   "fix [paths...]",
		Short: "Rewrite flagged migrations into their safe form",
		Long: `Fix applies deterministic rewrites (CONCURRENTLY, NOT VALID with a
follow-up VALIDATE migration, UNIQUE via index, jsonb) and, with --ai, asks a
Claude model to rewrite the findings that need judgement (renames via new
columns, batched backfills, NOT NULL via check constraints). Every proposal is
validated and re-linted before it is written.

--ai reads ANTHROPIC_API_KEY (and optionally ANTHROPIC_BASE_URL) from the
environment. Exit codes: 0 nothing left to fix, 1 findings remain, 2 error.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			proj, err := config.Load(".", *configPath)
			if err != nil {
				return err
			}
			runs, err := resolveRuns(cmd, args, &fl, *configPath)
			if err != nil {
				return err
			}
			out := fixOutput{Files: map[string]string{}, Diffs: map[string]string{}}
			remaining := &lint.Result{}
			for _, r := range runs {
				var before map[string]string
				if useAI {
					if !proj.AI.Enabled && os.Getenv("SAFE_SQL_AI_FORCE") == "" {
						return fmt.Errorf("ai.enabled is false in %s; set it to true (or SAFE_SQL_AI_FORCE=1 for a one-off local run)", proj.ConfigPathOrCwd())
					}
					provider, err := providerFor(proj, model)
					if err != nil {
						return err
					}
					det := true
					if proj.AI.DeterministicFirst != nil {
						det = *proj.AI.DeterministicFirst
					}
					before = snapshot(r.paths)
					res, err := ai.Run(context.Background(), r.paths, ai.Options{
						Provider: provider, Lint: r.opts, Rules: proj.AI.Rules, MaxIterations: firstPositive(maxIter, proj.AI.MaxIterations),
						AllowDisableAnnotations: allowDisable || proj.AI.AllowDisableAnnotations,
						SchemaDirs:              r.paths, DeterministicFirst: det, DryRun: dryRun,
					})
					if err != nil {
						return err
					}
					for _, ch := range res.Deterministic {
						out.Notes = append(out.Notes, noteFor(ch))
					}
					out.Summaries = append(out.Summaries, res.Summaries...)
					out.Declined = append(out.Declined, res.Declined...)
					out.Rejected = append(out.Rejected, res.Rejected...)
					out.Model = res.Model
					for p, c := range res.Files {
						out.Files[p] = c
					}
					mergeRemaining(remaining, res.Remaining)
				} else {
					res, err := fix.Run(r.paths, fix.Options{Lint: r.opts, Only: fl.only, DryRun: dryRun})
					if err != nil {
						return err
					}
					for _, ch := range res.Changes {
						out.Notes = append(out.Notes, noteFor(ch))
					}
					for _, u := range res.Unfixed {
						out.Unfixed = append(out.Unfixed, fmt.Sprintf("%s:%d %s: %s", u.File, u.Line, u.RuleID, u.Message))
					}
					for p, c := range res.After {
						if res.Before[p] != c {
							out.Files[p] = c
						}
					}
					before = res.Before
					mergeRemaining(remaining, res.Remaining)
				}
				for p, c := range out.Files {
					if orig, ok := before[p]; ok {
						out.Modified = appendUnique(out.Modified, p)
						out.Diffs[p] = fix.UnifiedDiff(p, orig, c)
					} else {
						out.Created = appendUnique(out.Created, p)
						out.Diffs[p] = fix.UnifiedDiff(p, "", c)
					}
				}
			}
			sort.Strings(out.Modified)
			sort.Strings(out.Created)
			out.Remaining = len(remaining.Findings)

			w := cmd.OutOrStdout()
			switch fl.format {
			case "json":
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				if err := enc.Encode(out); err != nil {
					return err
				}
			default:
				for _, p := range append(append([]string{}, out.Modified...), out.Created...) {
					fmt.Fprint(w, out.Diffs[p])
				}
				for _, n := range out.Notes {
					fmt.Fprintln(w, "note:", n)
				}
				for _, s := range out.Summaries {
					fmt.Fprintln(w, "model:", s)
				}
				for _, d := range out.Declined {
					fmt.Fprintln(w, "model declined:", d)
				}
				for _, r := range out.Rejected {
					fmt.Fprintln(w, "rejected proposal:", r)
				}
				if dryRun {
					fmt.Fprintf(w, "\n(dry run) %d file(s) would change, %d created.\n", len(out.Modified), len(out.Created))
				} else {
					fmt.Fprintf(w, "\n%d file(s) changed, %d created.\n", len(out.Modified), len(out.Created))
				}
				if len(remaining.Findings) > 0 {
					fmt.Fprintf(w, "\nStill unsafe after fixing:\n")
					_ = report.Write(w, remaining, report.FormatHuman)
				} else {
					fmt.Fprintln(w, "No unsafe operations remain.")
				}
			}
			if len(remaining.Findings) > 0 {
				*code = 1
			}
			return nil
		},
	}
	addLintFlags(cmd, &fl)
	cmd.Flags().BoolVar(&useAI, "ai", false, "use a Claude model for findings without a deterministic fix")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show the changes without writing files")
	cmd.Flags().StringVar(&model, "model", "", "model id for --ai (default: ai.model in config)")
	cmd.Flags().IntVar(&maxIter, "max-iterations", 0, "propose/re-lint rounds for --ai (default: ai.max_iterations)")
	cmd.Flags().BoolVar(&allowDisable, "allow-disable-annotations", false, "let --ai proposals contain safe_sql:disable")
	return cmd
}

func providerFor(proj *config.Project, model string) (ai.Provider, error) {
	if os.Getenv("SAFE_SQL_AI_FAKE") != "" {
		return fakeFromEnv()
	}
	switch proj.AI.Provider {
	case "", "anthropic":
		if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
			fmt.Fprintln(os.Stderr, "safe_sql: ANTHROPIC_API_KEY is not set; relying on an `ant auth` profile if one exists")
		}
		return ai.NewAnthropic(firstNonEmpty(model, proj.AI.Model)), nil
	}
	return nil, fmt.Errorf("unknown ai.provider %q", proj.AI.Provider)
}

// fakeFromEnv builds a scripted provider from SAFE_SQL_AI_FAKE, a path to a
// JSON file holding a list of responses ({"proposal": {...}} or {"text": "..."}).
// Used by CI to exercise the whole flow without network.
func fakeFromEnv() (ai.Provider, error) {
	b, err := os.ReadFile(os.Getenv("SAFE_SQL_AI_FAKE"))
	if err != nil {
		return nil, fmt.Errorf("SAFE_SQL_AI_FAKE: %w", err)
	}
	var responses []*ai.Response
	if err := json.Unmarshal(b, &responses); err != nil {
		return nil, fmt.Errorf("SAFE_SQL_AI_FAKE: %w", err)
	}
	for _, r := range responses {
		if r.Model == "" {
			r.Model = "fake"
		}
	}
	return &ai.Fake{Responses: responses}, nil
}

func noteFor(ch fix.Change) string {
	s := fmt.Sprintf("%s:%d %s fixed", ch.Finding.File, ch.Finding.Line, ch.Finding.RuleID)
	if ch.Note != "" {
		s += " (" + ch.Note + ")"
	}
	return s
}

// snapshot reads every migration under paths before anything is written, so
// diffs and the modified/created split are computed against the originals.
func snapshot(paths []string) map[string]string {
	out := map[string]string{}
	names, err := migrate.Discover(paths)
	if err != nil {
		return out
	}
	for _, p := range names {
		if b, err := os.ReadFile(p); err == nil {
			out[p] = string(b)
		}
	}
	return out
}

func mergeRemaining(dst, src *lint.Result) {
	if src == nil {
		return
	}
	dst.Findings = append(dst.Findings, src.Findings...)
	dst.Failures = append(dst.Failures, src.Failures...)
	dst.Files = append(dst.Files, src.Files...)
}

func firstPositive(xs ...int) int {
	for _, x := range xs {
		if x > 0 {
			return x
		}
	}
	return 3
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}
