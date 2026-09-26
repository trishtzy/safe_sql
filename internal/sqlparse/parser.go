package sqlparse

import (
	"fmt"
	"sort"
	"strings"
)

// Parser turns SQL source into Statements.
type Parser interface {
	// Parse returns every statement in src in order. A syntax error that the
	// engine cannot recover from is returned as *ParseError.
	Parse(src string) ([]Statement, error)
}

// ParseError is an unrecoverable syntax error with a position.
type ParseError struct {
	Message string
	Offset  int // byte offset in src, -1 if unknown
	Line    int // 1-based, 0 if unknown
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Message)
	}
	return e.Message
}

var registry = map[Engine]Parser{}

// Register makes a Parser available through ForEngine. Engine packages call
// it from init(); the CLI blank-imports them.
func Register(e Engine, p Parser) { registry[e] = p }

// ForEngine returns the registered parser for e.
func ForEngine(e Engine) (Parser, error) {
	p, ok := registry[e]
	if !ok {
		return nil, fmt.Errorf("no parser registered for engine %q (supported: %s)", e, strings.Join(Engines(), ", "))
	}
	return p, nil
}

// Engines lists registered engines, sorted.
func Engines() []string {
	out := make([]string, 0, len(registry))
	for e := range registry {
		out = append(out, string(e))
	}
	sort.Strings(out)
	return out
}

// LineAt returns the 1-based line number of byte offset off in src.
func LineAt(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	if off < 0 {
		off = 0
	}
	return 1 + strings.Count(src[:off], "\n")
}

// CommentsBetween extracts comment lines from src[from:to]. Each returned
// entry is the comment body with its marker (-- or /* */) removed and
// whitespace trimmed. Block comments contribute one entry per line.
func CommentsBetween(src string, from, to int) []string {
	if from < 0 {
		from = 0
	}
	if to > len(src) {
		to = len(src)
	}
	if from >= to {
		return nil
	}
	var out []string
	s := src[from:to]
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "--"):
			end := strings.IndexByte(s[i:], '\n')
			var body string
			if end < 0 {
				body = s[i+2:]
				i = len(s)
			} else {
				body = s[i+2 : i+end]
				i += end + 1
			}
			out = append(out, strings.TrimSpace(body))
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i:], "*/")
			var body string
			if end < 0 {
				body = s[i+2:]
				i = len(s)
			} else {
				body = s[i+2 : i+end]
				i += end + 2
			}
			for _, line := range strings.Split(body, "\n") {
				line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
				if line != "" {
					out = append(out, line)
				}
			}
		default:
			i++
		}
	}
	return out
}

// TrimStatement strips leading/trailing whitespace and a trailing semicolon,
// returning the trimmed text and the number of leading bytes removed.
func TrimStatement(s string) (string, int) {
	lead := len(s) - len(strings.TrimLeft(s, " \t\r\n"))
	t := strings.TrimRight(strings.TrimLeft(s, " \t\r\n"), " \t\r\n;")
	return t, lead
}
