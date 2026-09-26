package fix

import (
	"strings"
	"testing"
)

func TestUnifiedDiffAndSuggestions(t *testing.T) {
	before := "a\nb\nc\nd\ne\n"
	after := "a\nB\nc\nd\ne\nf\n"
	d := UnifiedDiff("x.sql", before, after)
	for _, want := range []string{"--- x.sql", "+++ x.sql", "-b\n", "+B\n", "+f\n", "@@ -1,"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if UnifiedDiff("x", "same", "same") != "" {
		t.Error("identical texts should produce no diff")
	}
	if !strings.HasPrefix(UnifiedDiff("n.sql", "", "new\n"), "--- /dev/null\n+++ n.sql\n") {
		t.Error("new file header")
	}

	s := Suggestions(before, after)
	if len(s) != 2 {
		t.Fatalf("suggestions = %+v", s)
	}
	if s[0].StartLine != 2 || s[0].EndLine != 2 || s[0].Replacement != "B" {
		t.Errorf("s0 = %+v", s[0])
	}
	// Insertion after the last line anchors on line 5 and repeats it.
	if s[1].StartLine != 5 || s[1].EndLine != 5 || s[1].Replacement != "e\nf" {
		t.Errorf("s1 = %+v", s[1])
	}
	// Deleting a line yields an empty replacement for that line.
	s = Suggestions("a\nb\nc\n", "a\nc\n")
	if len(s) != 1 || s[0].StartLine != 2 || s[0].EndLine != 2 || s[0].Replacement != "" {
		t.Errorf("delete = %+v", s)
	}
}
