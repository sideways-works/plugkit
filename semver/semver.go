// Package semver parses versions and the ranges a plugin and its host declare for each other:
// comparators separated by spaces must all hold, `||` separates alternatives, `!=` excludes,
// a bare version is exact, `*` is any. A pre-release matches only an alternative that names a
// pre-release of the same major.minor.patch, as npm does.
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrVersion = errors.New("version")

// Version is a semantic version; a leading v is accepted and build metadata kept but never
// compared.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

func Parse(s string) (Version, error) {
	parts, pre, build, err := split(s)
	if err != nil {
		return Version{}, err
	}

	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w %q: needs major.minor.patch", ErrVersion, s)
	}

	var v Version

	for i, p := range parts {
		n, err := number(p)
		if err != nil {
			return Version{}, fmt.Errorf("%w %q: %w", ErrVersion, s, err)
		}

		*[]*uint64{&v.Major, &v.Minor, &v.Patch}[i] = n
	}

	v.Pre, v.Build = pre, build

	return v, nil
}

// split cuts a version into its dot-separated numbers, pre-release identifiers and build.
func split(s string) ([]string, []string, string, error) {
	rest := strings.TrimPrefix(s, "v")

	rest, build, hasBuild := strings.Cut(rest, "+")
	if hasBuild && !identifiers(build, false) {
		return nil, nil, "", fmt.Errorf("%w %q: build %q", ErrVersion, s, build)
	}

	core, preText, hasPre := strings.Cut(rest, "-")

	var pre []string

	if hasPre {
		if !identifiers(preText, true) {
			return nil, nil, "", fmt.Errorf("%w %q: pre-release %q", ErrVersion, s, preText)
		}

		pre = strings.Split(preText, ".")
	}

	if core == "" {
		return nil, nil, "", fmt.Errorf("%w %q: empty", ErrVersion, s)
	}

	return strings.Split(core, "."), pre, build, nil
}

func identifiers(s string, strictNumbers bool) bool {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
			return false
		}

		if strictNumbers && numeric(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}

	return true
}

func numeric(id string) bool { return strings.Trim(id, "0123456789") == "" }

var errNumber = errors.New("a number without leading zeros")

func number(s string) (uint64, error) {
	if s == "" || !numeric(s) || len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("%w, not %q", errNumber, s)
	}

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w, not %q", errNumber, s)
	}

	return n, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}

	if v.Build != "" {
		s += "+" + v.Build
	}

	return s
}

func (v Version) Prerelease() bool { return len(v.Pre) > 0 }

func (v Version) sameRelease(o Version) bool {
	return v.Major == o.Major && v.Minor == o.Minor && v.Patch == o.Patch
}

// Compare orders by precedence: -1, 0 or 1. Build metadata doesn't count.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}

			return 1
		}
	}

	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}

	for i := range min(len(v.Pre), len(o.Pre)) {
		if c := compareIdentifier(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}

	switch {
	case len(v.Pre) < len(o.Pre):
		return -1
	case len(v.Pre) > len(o.Pre):
		return 1
	}

	return 0
}

func compareIdentifier(a, b string) int {
	an, bn := numeric(a), numeric(b)

	switch {
	case an && bn:
		x, _ := strconv.ParseUint(a, 10, 64) //nolint:errcheck // checked numeric when parsed
		y, _ := strconv.ParseUint(b, 10, 64) //nolint:errcheck // checked numeric when parsed

		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}

		return 0
	case an:
		return -1
	case bn:
		return 1
	}

	return strings.Compare(a, b)
}
