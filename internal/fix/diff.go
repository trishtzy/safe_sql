package fix

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a minimal unified diff between two texts. It is meant
// for terminal display and pull request comments, not for patch(1).
func UnifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	a := splitLines(before)
	b := splitLines(after)
	ops := diffLines(a, b)
	var out strings.Builder
	if before == "" {
		fmt.Fprintf(&out, "--- /dev/null\n+++ %s\n", path)
	} else {
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", path, path)
	}
	// Group into hunks with 3 lines of context.
	const ctx = 3
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(i-ctx, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// Extend through context if another change follows within 2*ctx.
			j := end
			for j < len(ops) && ops[j].kind == ' ' && j-end < 2*ctx {
				j++
			}
			if j < len(ops) && ops[j].kind != ' ' {
				end = j
				continue
			}
			break
		}
		hunkEnd := min(end+ctx, len(ops))
		aStart, bStart := ops[start].ai, ops[start].bi
		var aLen, bLen int
		var body strings.Builder
		for _, o := range ops[start:hunkEnd] {
			switch o.kind {
			case ' ':
				aLen++
				bLen++
			case '-':
				aLen++
			case '+':
				bLen++
			}
			body.WriteByte(o.kind)
			body.WriteString(o.text)
			body.WriteByte('\n')
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n%s", aStart+1, aLen, bStart+1, bLen, body.String())
		i = hunkEnd
	}
	return out.String()
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
	ai   int // index into a at this op (for '-' and ' ')
	bi   int // index into b at this op (for '+' and ' ')
}

// diffLines computes an LCS-based line diff. Files here are small.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i], i, j})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j], i, j})
	}
	return ops
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
