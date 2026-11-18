package container

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTheCommandLineMountsPassesTheEnvironmentAndPullsByDigest(t *testing.T) {
	r := Run{
		Tool: "docker", Image: "busybox@sha256:" + strings.Repeat("ab", 32), Name: "runner-unit-batch-1", Dir: "/w/unit/build",
		Mounts: []Mount{{Host: "/w/unit", At: "/w/unit"}, {Host: "/repo/.git", At: "/repo/.git", ReadOnly: true}},
		Env:    map[string]string{"SUITE": "unit", "B": "2", "A": "1"},
		User:   "1000:1000", Privileged: true,
		Cmd: []string{"/w/unit/build/tools/runner", "slice", "run"},
	}

	got, err := r.Args()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"run", "--rm", "--name", "runner-unit-batch-1", "--network", "bridge", "--workdir", "/w/unit/build",
		"--privileged", "--user", "1000:1000",
		"--volume", "/w/unit:/w/unit", "--volume", "/repo/.git:/repo/.git:ro",
		"--env", "A=1", "--env", "B=2", "--env", "SUITE=unit",
		"--entrypoint", "/w/unit/build/tools/runner", "busybox@sha256:" + strings.Repeat("ab", 32),
		"slice", "run",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("args:\n got %q\nwant %q", got, want)
	}
}

// A step before the suite's (installing what it declares) runs in the same container by a shell,
// ahead of the command, and what runs after it whatever its status.
func TestBeforeAndAfterRunInTheSameContainer(t *testing.T) {
	r := Run{Tool: "podman", Image: "alpine:3", Name: "n", Dir: "/", Before: []string{"apk add git"}, After: []string{"echo done"}, Cmd: []string{"make", "test"}}

	got, err := r.Args()
	if err != nil {
		t.Fatal(err)
	}

	i := slices.Index(got, "alpine:3")
	if i < 2 || !slices.Equal(got[i-2:i], []string{"--entrypoint", "sh"}) || got[i+1] != "-c" || !slices.Equal(got[i+3:], []string{"sh", "make", "test"}) {
		t.Fatalf("args: %q", got)
	}

	if script := got[i+2]; !strings.Contains(script, "set -e\napk add git\nset +e\n\"$@\"\nrc=$?\necho done\nexit $rc") {
		t.Fatalf("script: %q", script)
	}
}

func TestAPathWithAColonIsRefused(t *testing.T) {
	r := Run{Tool: "docker", Image: "busybox", Name: "n", Dir: "/", Mounts: []Mount{{Host: "/a:b", At: "/a:b"}}}

	if _, err := r.Args(); err == nil {
		t.Fatal("no error")
	}
}

func TestFindTakesTheNamedTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if _, err := Find(""); !errors.Is(err, ErrNoTool) {
		t.Fatalf("no tool: %v", err)
	}

	if _, err := Find("podman"); err == nil || !strings.HasPrefix(err.Error(), "podman: ") {
		t.Fatalf("a tool that isn't on PATH: %v", err)
	}
}

// One container from a light image, when docker or podman runs here: root, as on CI, what it wrote
// is the user's once handed back, and only the environment given reaches it.
func TestATinySuiteRunsInBusybox(t *testing.T) {
	tool, err := Find("")
	if err != nil || !Reachable(t.Context(), tool) {
		t.Skip("no container tool here, or it isn't running")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	var log bytes.Buffer
	if _, err := Ensure(ctx, tool, "busybox:1.37.0", &log); err != nil {
		t.Skipf("busybox can't be pulled here: %v\n%s", err, log.String())
	}

	dir := t.TempDir()
	user, userns, handBack := Owner(ctx, tool, false)

	r := Run{
		Tool: tool, Image: "busybox:1.37.0", Name: Name("plugkit-test"), Dir: dir,
		Mounts: []Mount{{Host: dir, At: dir}}, Env: map[string]string{"GREETING": "hello"}, User: user, UserNS: userns,
		Cmd: []string{"sh", "-c", `mkdir sub && echo "$GREETING ${SECRET_TOKEN:-unset} $(id -u)" > sub/out.txt`},
	}

	t.Setenv("SECRET_TOKEN", "never-passed")

	for _, run := range []Run{r, HandBack(r, dir)} {
		if run.Name != r.Name && !handBack {
			continue
		}

		cmd, err := run.Command(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", run.Name, err, out)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "sub", "out.txt"))
	if err != nil || string(data) != "hello unset 0\n" {
		t.Fatalf("out.txt: %q %v", data, err)
	}

	// the test's directory goes after: the file and its directory must be this user's to remove
	if err := os.WriteFile(filepath.Join(dir, "sub", "mine.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}

	if err := exec.CommandContext(ctx, tool, "inspect", r.Name).Run(); err == nil {
		t.Fatalf("the container %s is still there", r.Name)
	}
}

func TestAKeptContainerIsNotRemovedWhenItEnds(t *testing.T) {
	got, err := Run{Tool: "docker", Image: "alpine", Name: "n", Dir: "/", Keep: true, Cmd: []string{"true"}}.Args()
	if err != nil {
		t.Fatal(err)
	}

	if slices.Contains(got, "--rm") || got[0] != "run" || got[1] != "--name" {
		t.Fatalf("args: %q", got)
	}
}

// A sandboxed call runs without a network on a read-only image, writing only to what is mounted.
func TestASandboxedCallHasNoNetworkAndAReadOnlyRoot(t *testing.T) {
	r := Run{
		Tool: "podman", Image: "alpine:3", Name: "call", Dir: "/w", Network: "none", ReadOnlyRoot: true,
		Mounts: []Mount{{Host: "/in", At: "/in", ReadOnly: true}, {Host: "/w", At: "/w"}},
		Cmd:    []string{"plugin", "describe"},
	}

	got, err := r.Args()
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"run", "--rm", "--name", "call", "--network", "none", "--workdir", "/w", "--read-only",
		"--volume", "/in:/in:ro", "--volume", "/w:/w",
		"--entrypoint", "plugin", "alpine:3", "describe",
	}

	if !slices.Equal(got, want) {
		t.Fatalf("args:\n got %q\nwant %q", got, want)
	}
}
