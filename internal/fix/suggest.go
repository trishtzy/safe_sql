package fix

import "strings"

// Suggestion is a GitHub review suggestion: replace lines [StartLine, EndLine]
// of the current file with Body.
type Suggestion struct {
	StartLine, EndLine int
	Replacement        string
}

// Suggestions converts a before/after pair into review suggestions, one per
// changed region. Pure insertions are attached to the preceding line.
func Suggestions(before, after string) []Suggestion {
	ops := diffLines(splitLines(before), splitLines(after))
	var out []Suggestion
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		j := i
		var removed, added []string
		for j < len(ops) && ops[j].kind != ' ' {
			if ops[j].kind == '-' {
				removed = append(removed, ops[j].text)
			} else {
				added = append(added, ops[j].text)
			}
			j++
		}
		start := ops[i].ai + 1 // 1-based old line
		end := start + len(removed) - 1
		if len(removed) == 0 {
			// Insertion: anchor on the previous line and repeat it.
			if ops[i].ai == 0 {
				start, end = 1, 1
				if len(splitLines(before)) == 0 {
					out = append(out, Suggestion{1, 1, strings.Join(added, "\n")})
					i = j
					continue
				}
				added = append([]string{splitLines(before)[0]}, added...)
			} else {
				start, end = ops[i].ai, ops[i].ai
				added = append([]string{splitLines(before)[ops[i].ai-1]}, added...)
			}
		}
		out = append(out, Suggestion{start, end, strings.Join(added, "\n")})
		i = j
	}
	return out
}
