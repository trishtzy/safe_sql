// Package github delivers fix results to a pull request: reactions,
// comments, reviews with suggestion blocks, and commits. It also decides
// whether a PR comment is allowed to trigger the AI flow, using settings from
// the base branch only.
package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// Client talks to the GitHub REST API for one repository.
type Client struct {
	Token string
	Repo  string // owner/name
	API   string // https://api.github.com
	HTTP  *http.Client
}

// FromEnv builds a client from GITHUB_TOKEN, GITHUB_REPOSITORY, GITHUB_API_URL.
func FromEnv() (*Client, error) {
	c := &Client{Token: os.Getenv("GITHUB_TOKEN"), Repo: os.Getenv("GITHUB_REPOSITORY"), API: os.Getenv("GITHUB_API_URL"), HTTP: http.DefaultClient}
	if c.API == "" {
		c.API = "https://api.github.com"
	}
	if c.Token == "" || c.Repo == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN and GITHUB_REPOSITORY are required")
	}
	return c, nil
}

func (c *Client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.API+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// PullRequest is the subset of PR metadata delivery needs.
type PullRequest struct {
	Number  int
	HeadSHA string
	HeadRef string
	BaseRef string
	// Fork is true when the head branch lives in another repository, where
	// GITHUB_TOKEN cannot push.
	Fork bool
}

// PR fetches pull request metadata.
func (c *Client) PR(number int) (*PullRequest, error) {
	var raw struct {
		Number int `json:"number"`
		Head   struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.do("GET", fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, number), nil, &raw); err != nil {
		return nil, err
	}
	return &PullRequest{Number: raw.Number, HeadSHA: raw.Head.SHA, HeadRef: raw.Head.Ref, BaseRef: raw.Base.Ref,
		Fork: raw.Head.Repo.FullName != "" && !strings.EqualFold(raw.Head.Repo.FullName, c.Repo)}, nil
}

// React adds a reaction (e.g. "eyes", "rocket") to an issue comment.
func (c *Client) React(commentID int64, content string) error {
	return c.do("POST", fmt.Sprintf("/repos/%s/issues/comments/%d/reactions", c.Repo, commentID), map[string]string{"content": content}, nil)
}

// Comment posts a comment on a pull request.
func (c *Client) Comment(pr int, body string) error {
	return c.do("POST", fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, pr), map[string]string{"body": body}, nil)
}

// ReviewComment is one inline review comment.
type ReviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

// Review posts a pull request review with inline comments.
func (c *Client) Review(pr int, commitSHA, body string, comments []ReviewComment) error {
	payload := map[string]any{"commit_id": commitSHA, "body": body, "event": "COMMENT", "comments": comments}
	return c.do("POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", c.Repo, pr), payload, nil)
}

// CommitAndPush commits the given paths on the current checkout as the
// safe_sql bot and pushes to the PR head branch.
func CommitAndPush(paths []string, message, headRef string) error {
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd.Run()
	}
	if err := run(append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	if err := run("-c", "user.name=safe_sql[bot]", "-c", "user.email=safe_sql[bot]@users.noreply.github.com", "commit", "-m", message); err != nil {
		return err
	}
	return run("push", "origin", "HEAD:"+headRef)
}
