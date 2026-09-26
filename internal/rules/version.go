package rules

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a database server version. Unknown versions (Known == false)
// are treated as the latest release: rules that need a minimum version
// assume it is met, matching strong_migrations' behaviour when the server
// version cannot be determined.
type Version struct {
	Major, Minor, Patch int
	Known               bool
}

// ParseVersion accepts "16", "16.2", "3.35.0". Empty returns an unknown version.
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Version{}, nil
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("invalid version %q", s)
	}
	v := Version{Known: true}
	nums := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("invalid version %q", s)
		}
		*nums[i] = n
	}
	return v, nil
}

// AtLeast reports whether v >= major.minor. Unknown versions report true.
func (v Version) AtLeast(major, minor int) bool {
	if !v.Known {
		return true
	}
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

func (v Version) String() string {
	if !v.Known {
		return "latest"
	}
	if v.Patch > 0 {
		return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	}
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}
