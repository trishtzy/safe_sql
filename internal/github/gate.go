package github

import (
	"fmt"
	"strings"
)

// GateInput is what a PR comment must satisfy to trigger an AI fix. The
// settings must come from the base branch's safe_sql.yaml: a pull request
// can edit its own copy of the file, so the PR head is never trusted for
// gating.
type GateInput struct {
	Enabled             bool
	Trigger             string
	AllowedAssociations []string
	CommentBody         string
	AuthorAssociation   string
	IsPullRequest       bool
}

// Gate returns nil when the comment may trigger a run, else the reason not.
func Gate(in GateInput) error {
	if !in.IsPullRequest {
		return fmt.Errorf("comment is not on a pull request")
	}
	if !in.Enabled {
		return fmt.Errorf("ai.enabled is false in safe_sql.yaml on the base branch")
	}
	trigger := in.Trigger
	if trigger == "" {
		trigger = "@safe_sql_ai"
	}
	if !containsWord(in.CommentBody, trigger) {
		return fmt.Errorf("comment does not mention %s", trigger)
	}
	assoc := strings.ToUpper(strings.TrimSpace(in.AuthorAssociation))
	for _, a := range in.AllowedAssociations {
		if strings.EqualFold(a, assoc) {
			return nil
		}
	}
	return fmt.Errorf("commenter association %q is not in ai.allowed_associations %v", assoc, in.AllowedAssociations)
}

// containsWord matches trigger as a whole token so "@safe_sql_ai_bot" does
// not count for "@safe_sql_ai".
func containsWord(body, trigger string) bool {
	for i := 0; i+len(trigger) <= len(body); i++ {
		if body[i:i+len(trigger)] != trigger {
			continue
		}
		before := i == 0 || !isWord(body[i-1])
		after := i+len(trigger) == len(body) || !isWord(body[i+len(trigger)])
		if before && after {
			return true
		}
	}
	return false
}

func isWord(b byte) bool {
	return b == '_' || b == '-' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
