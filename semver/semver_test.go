package semver

import (
	"errors"
	"slices"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{in: "1.2.3", want: "1.2.3"},
		{in: "v1.2.3", want: "1.2.3"},
		{in: "1.2.3-rc.1", want: "1.2.3-rc.1"},
		{in: "1.2.3-rc.1+build.5", want: "1.2.3-rc.1+build.5"},
		{in: "0.0.0", want: "0.0.0"},
		{in: "1.2", bad: true},
		{in: "1", bad: true},
		{in: "1.2.3.4", bad: true},
		{in: "01.2.3", bad: true},
		{in: "1.2.3-01", bad: true},
		{in: "1.2.3-", bad: true},
		{in: "1.2.3-a..b", bad: true},
		{in: "1.2.x", bad: true},
		{in: "", bad: true},
		{in: "a.b.c", bad: true},
	} {
		v, err := Parse(tc.in)

		switch {
		case tc.bad && !errors.Is(err, ErrVersion):
			t.Errorf("%q: parsed as %s, %v", tc.in, v, err)
		case !tc.bad && (err != nil || v.String() != tc.want):
			t.Errorf("%q: %s, %v", tc.in, v, err)
		}
	}
}

func TestCompare(t *testing.T) {
	// semver.org's precedence example, lowest first
	order := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2",
		"1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0", "10.0.0",
	}

	vs := make([]Version, 0, len(order))
	for _, s := range slices.Backward(order) {
		vs = append(vs, must(t, s))
	}

	slices.SortFunc(vs, Version.Compare)

	for i, v := range vs {
		if v.String() != order[i] {
			t.Fatalf("sorted %v", vs)
		}
	}

	if must(t, "1.0.0+a").Compare(must(t, "1.0.0+b")) != 0 {
		t.Error("build metadata counts")
	}
}

func must(t *testing.T, s string) Version {
	t.Helper()

	v, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func TestRange(t *testing.T) {
	for _, tc := range []struct {
		rng     string
		in, out []string
	}{
		{rng: ">=1.2 <2", in: []string{"1.2.0", "1.9.9", "1.2.0+b"}, out: []string{"1.1.9", "2.0.0", "2.0.0-rc.1", "1.5.0-rc.1"}},
		{rng: ">=1.2 <1.4 || >=1.5 <2", in: []string{"1.2.0", "1.3.9", "1.5.0", "1.9.0"}, out: []string{"1.4.0", "1.4.9", "2.0.0", "1.1.0"}},
		{rng: ">=1.0 <2 !=1.3.2", in: []string{"1.3.1", "1.3.3"}, out: []string{"1.3.2"}},
		{rng: "!=1.3", in: []string{"1.2.9", "1.4.0"}, out: []string{"1.3.0", "1.3.7", "1.3.0-rc.1"}},
		{rng: "1.2.3", in: []string{"1.2.3", "v1.2.3", "1.2.3+b"}, out: []string{"1.2.4", "1.2.3-rc.1"}},
		{rng: "=1.2.3", in: []string{"1.2.3"}, out: []string{"1.2.2"}},
		{rng: "1.2", in: []string{"1.2.0", "1.2.9"}, out: []string{"1.3.0", "1.1.9", "1.2.0-rc.1"}},
		{rng: "1", in: []string{"1.0.0", "1.9.9"}, out: []string{"2.0.0", "0.9.0"}},
		{rng: "1.x", in: []string{"1.4.0"}, out: []string{"2.0.0"}},
		{rng: "1.2.*", in: []string{"1.2.7"}, out: []string{"1.3.0"}},
		{rng: "*", in: []string{"0.0.0", "1.2.3", "99.0.0"}, out: []string{"1.0.0-rc.1"}},
		{rng: ">=1.3.0-rc.1", in: []string{"1.3.0-rc.1", "1.3.0-rc.2", "1.3.0", "1.4.0"}, out: []string{"1.3.0-beta", "1.4.0-rc.1", "1.2.9"}},
		{rng: ">=1.3.0-rc.1 <2", in: []string{"1.3.0-rc.5", "1.9.0"}, out: []string{"1.5.0-rc.1", "2.0.0-rc.1"}},
		{rng: "<1.3.0-rc.2", in: []string{"1.3.0-rc.1", "1.2.0"}, out: []string{"1.3.0-rc.2", "1.3.0", "1.2.0-rc.1"}},
		{rng: ">=1.3.0-rc.1 || >=2.0.0-beta", in: []string{"2.0.0-beta.1"}, out: []string{"2.1.0-beta"}},
		{rng: ">1.2", in: []string{"1.3.0"}, out: []string{"1.2.9", "1.3.0-rc.1"}},
		{rng: ">1.2.3", in: []string{"1.2.4"}, out: []string{"1.2.3"}},
		{rng: "<=1.2", in: []string{"1.2.9"}, out: []string{"1.3.0"}},
		{rng: "<=1.2.3", in: []string{"1.2.3"}, out: []string{"1.2.4"}},
		{rng: "<1.2", in: []string{"1.1.9"}, out: []string{"1.2.0", "1.2.0-rc.1"}},
		{rng: ">= 1.2  < 2", in: []string{"1.5.0"}, out: []string{"2.0.0"}},
		{rng: ">=v1.2.0", in: []string{"1.2.0"}, out: []string{"1.1.0"}},
		{rng: "<*", out: []string{"0.0.0", "1.0.0"}},
		{rng: ">*", out: []string{"0.0.0", "1.0.0"}},
		{rng: "<=*", in: []string{"0.0.0", "1.0.0"}},
		{rng: "!=1.3.0-rc.1", in: []string{"1.3.0"}, out: []string{"1.3.0-rc.1", "1.3.0-rc.2"}},
	} {
		r, err := ParseRange(tc.rng)
		if err != nil {
			t.Errorf("%q: %v", tc.rng, err)

			continue
		}

		for _, s := range tc.in {
			if !r.Contains(must(t, s)) {
				t.Errorf("%q leaves out %s", tc.rng, s)
			}
		}

		for _, s := range tc.out {
			if r.Contains(must(t, s)) {
				t.Errorf("%q holds %s", tc.rng, s)
			}
		}
	}
}

func TestRangeRefusals(t *testing.T) {
	for _, rng := range []string{
		"", " ", "1 ||", "|| 1", "^1.2", "~1.2", "1.2 - 1.4", ">=", "!=*", ">=1.2.x.4",
		"1.2-rc.1", ">=a", "1.x.3", "1..2", ">=1.2.3-", "=>1.2",
	} {
		if r, err := ParseRange(rng); !errors.Is(err, ErrRange) {
			t.Errorf("%q: parsed as %v, %v", rng, r.alts, err)
		}
	}
}
