package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trishtzy/safe_sql/internal/config"
	"github.com/trishtzy/safe_sql/internal/fix"
	gh "github.com/trishtzy/safe_sql/internal/github"
)

func newGithubCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "github", Short: "Helpers used by the GitHub Action (gate, react, deliver)", Hidden: true}
	cmd.AddCommand(newGateCmd(), newReactCmd(), newDeliverCmd(), newModeCmd())
	return cmd
}

func newGateCmd() *cobra.Command {
	var baseConfig, body, association string
	var isPR bool
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Exit 0 if a PR comment may trigger the AI fix, using safe_sql.yaml from the base branch",
		RunE: func(cmd *cobra.Command, args []string) error {
			ai, err := config.LoadAI(baseConfig)
			if err != nil {
				return err
			}
			err = gh.Gate(gh.GateInput{Enabled: ai.Enabled, Trigger: ai.Trigger, AllowedAssociations: ai.AllowedAssociations,
				CommentBody: body, AuthorAssociation: association, IsPullRequest: isPR})
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "skip:", err)
				os.Exit(78) // GitHub's conventional "neutral" code; the action treats it as skip
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&baseConfig, "base-config", "", "safe_sql.yaml as extracted from the base branch (required)")
	cmd.Flags().StringVar(&body, "body", "", "comment body")
	cmd.Flags().StringVar(&association, "association", "", "commenter author_association")
	cmd.Flags().BoolVar(&isPR, "is-pr", false, "the comment is on a pull request")
	_ = cmd.MarkFlagRequired("base-config")
	return cmd
}

func newModeCmd() *cobra.Command {
	var baseConfig string
	cmd := &cobra.Command{
		Use: "mode", Short: "Print ai.mode from the given config",
		RunE: func(cmd *cobra.Command, args []string) error {
			ai, err := config.LoadAI(baseConfig)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), ai.Mode)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseConfig, "base-config", "", "safe_sql.yaml from the base branch")
	return cmd
}

func newReactCmd() *cobra.Command {
	var commentID int64
	var content string
	cmd := &cobra.Command{
		Use: "react", Short: "Add a reaction to the triggering comment",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := gh.FromEnv()
			if err != nil {
				return err
			}
			return c.React(commentID, content)
		},
	}
	cmd.Flags().Int64Var(&commentID, "comment-id", 0, "issue comment id")
	cmd.Flags().StringVar(&content, "content", "eyes", "reaction")
	return cmd
}

func newDeliverCmd() *cobra.Command {
	var pr int
	var mode, input, commitMessage string
	cmd := &cobra.Command{
		Use:   "deliver",
		Short: "Deliver `safe_sql fix --format json` output to a pull request",
		Long: `Modes: commit (push a commit to the PR branch; falls back to suggest for
forks), suggest (a review with suggestion blocks, new files in the review
body), comment (one comment with the diffs). Always ends with a summary.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := gh.FromEnv()
			if err != nil {
				return err
			}
			b, err := os.ReadFile(input)
			if err != nil {
				return err
			}
			var out fixOutput
			if err := json.Unmarshal(b, &out); err != nil {
				return fmt.Errorf("%s: %w", input, err)
			}
			info, err := c.PR(pr)
			if err != nil {
				return err
			}
			changed := append(append([]string{}, out.Modified...), out.Created...)
			if mode == "commit" && info.Fork {
				fmt.Fprintln(cmd.ErrOrStderr(), "safe_sql: PR is from a fork; cannot push, falling back to suggest")
				mode = "suggest"
			}
			delivered := ""
			switch {
			case len(changed) == 0:
				delivered = "No files were changed."
			case mode == "commit":
				if err := gh.CommitAndPush(changed, commitMessage, info.HeadRef); err != nil {
					return err
				}
				delivered = fmt.Sprintf("Pushed a commit to `%s` changing %d file(s).", info.HeadRef, len(changed))
			case mode == "suggest":
				var comments []gh.ReviewComment
				for _, p := range out.Modified {
					// fix already rewrote the working tree, so the original
					// content must come from the PR head commit, not disk.
					before, err := exec.Command("git", "show", "HEAD:"+filepath.ToSlash(p)).Output()
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "safe_sql: cannot read %s at HEAD: %v\n", p, err)
						continue
					}
					for _, s := range fix.Suggestions(string(before), out.Files[p]) {
						rc := gh.ReviewComment{Path: p, Line: s.EndLine, Side: "RIGHT", Body: "```suggestion\n" + s.Replacement + "\n```"}
						if s.StartLine != s.EndLine {
							rc.StartLine, rc.StartSide = s.StartLine, "RIGHT"
						}
						comments = append(comments, rc)
					}
				}
				body := "safe_sql proposes these changes as suggestions."
				for _, p := range out.Created {
					body += fmt.Sprintf("\n\nNew file `%s`:\n\n```sql\n%s\n```", p, strings.TrimRight(out.Files[p], "\n"))
				}
				if err := c.Review(pr, info.HeadSHA, body, comments); err != nil {
					return err
				}
				delivered = fmt.Sprintf("Posted a review with %d suggestion(s) and %d new file(s).", len(comments), len(out.Created))
			default:
				var body strings.Builder
				body.WriteString("safe_sql proposes these changes:\n")
				for _, p := range changed {
					fmt.Fprintf(&body, "\n```diff\n%s```\n", out.Diffs[p])
				}
				if err := c.Comment(pr, body.String()); err != nil {
					return err
				}
				delivered = "Posted the diffs as a comment."
			}
			return c.Comment(pr, summaryComment(out, delivered))
		},
	}
	cmd.Flags().IntVar(&pr, "pr", 0, "pull request number")
	cmd.Flags().StringVar(&mode, "mode", "commit", "commit | suggest | comment")
	cmd.Flags().StringVar(&input, "input", "", "path to fix --format json output")
	cmd.Flags().StringVar(&commitMessage, "commit-message", "safe_sql: rewrite migration using a safer pattern", "commit message for mode=commit")
	_ = cmd.MarkFlagRequired("pr")
	_ = cmd.MarkFlagRequired("input")
	return cmd
}

func summaryComment(out fixOutput, delivered string) string {
	var b strings.Builder
	b.WriteString("## safe_sql AI fix\n\n")
	b.WriteString(delivered + "\n")
	if len(out.Notes) > 0 {
		b.WriteString("\n**Deterministic fixes**\n")
		for _, n := range out.Notes {
			b.WriteString("- " + n + "\n")
		}
	}
	if len(out.Summaries) > 0 {
		b.WriteString("\n**Model changes**\n")
		for _, s := range out.Summaries {
			b.WriteString("- " + s + "\n")
		}
	}
	if len(out.Declined) > 0 {
		b.WriteString("\n**Needs a human**\n")
		for _, d := range out.Declined {
			b.WriteString("- " + d + "\n")
		}
	}
	if len(out.Rejected) > 0 {
		b.WriteString("\n**Rejected proposals** (failed validation)\n")
		for _, r := range out.Rejected {
			b.WriteString("- " + r + "\n")
		}
	}
	if out.Remaining > 0 {
		fmt.Fprintf(&b, "\n%d finding(s) remain after fixing; run `safe_sql lint` locally for details.\n", out.Remaining)
	} else {
		b.WriteString("\nNo unsafe operations remain.\n")
	}
	if out.Model != "" {
		fmt.Fprintf(&b, "\n<sub>model: %s</sub>\n", out.Model)
	}
	return b.String()
}
