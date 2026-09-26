package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGate(t *testing.T) {
	base := GateInput{Enabled: true, Trigger: "@safe_sql_ai", AllowedAssociations: []string{"OWNER", "MEMBER"}, CommentBody: "please @safe_sql_ai fix", AuthorAssociation: "member", IsPullRequest: true}
	if err := Gate(base); err != nil {
		t.Errorf("allowed: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*GateInput)
	}{
		{"disabled", func(g *GateInput) { g.Enabled = false }},
		{"not a PR", func(g *GateInput) { g.IsPullRequest = false }},
		{"no mention", func(g *GateInput) { g.CommentBody = "fix it" }},
		{"partial mention", func(g *GateInput) { g.CommentBody = "@safe_sql_ai_bot go" }},
		{"contributor", func(g *GateInput) { g.AuthorAssociation = "CONTRIBUTOR" }},
		{"none", func(g *GateInput) { g.AuthorAssociation = "NONE" }},
	}
	for _, c := range cases {
		in := base
		c.mut(&in)
		if err := Gate(in); err == nil {
			t.Errorf("%s: expected rejection", c.name)
		}
	}
}

func TestClientCalls(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/7"):
			w.Write([]byte(`{"number":7,"head":{"sha":"abc","ref":"feature","repo":{"full_name":"other/fork"}},"base":{"ref":"main"}}`))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			if body["event"] != "COMMENT" || body["commit_id"] != "abc" {
				t.Errorf("review payload = %v", body)
			}
			w.WriteHeader(200)
		default:
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	c := &Client{Token: "tok", Repo: "me/repo", API: srv.URL, HTTP: srv.Client()}
	pr, err := c.PR(7)
	if err != nil || pr.HeadSHA != "abc" || !pr.Fork || pr.HeadRef != "feature" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	if err := c.React(5, "eyes"); err != nil {
		t.Error(err)
	}
	if err := c.Comment(7, "hi"); err != nil {
		t.Error(err)
	}
	if err := c.Review(7, "abc", "body", []ReviewComment{{Path: "a.sql", Line: 2, Side: "RIGHT", Body: "x"}}); err != nil {
		t.Error(err)
	}
	want := []string{"GET /repos/me/repo/pulls/7", "POST /repos/me/repo/issues/comments/5/reactions", "POST /repos/me/repo/issues/7/comments", "POST /repos/me/repo/pulls/7/reviews"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v", got)
	}
}
