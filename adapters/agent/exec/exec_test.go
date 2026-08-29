package exec

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/knograph/kno/core/errs"
)

// TestSplitCommandRule pins the argv-split rule: whitespace separates,
// NOTHING else is interpreted. In particular a quoted argument is not
// unquoted — it arrives literally, quote characters and all, and the help
// text says so.
func TestSplitCommandRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ref  string
		want []string
	}{
		{"one token", "cat", []string{"cat"}},
		{"program plus args", "sh testdata/good.sh", []string{"sh", "testdata/good.sh"}},
		{"runs of whitespace collapse", "sh   testdata/good.sh   --flag", []string{"sh", "testdata/good.sh", "--flag"}},
		{"tabs separate", "sh\ttestdata/good.sh", []string{"sh", "testdata/good.sh"}},
		{
			"quotes are literal characters", `sh testdata/args.sh "hello world"`,
			[]string{"sh", "testdata/args.sh", `"hello`, `world"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitCommand(tt.ref)
			if len(got) != len(tt.want) {
				t.Fatalf("splitCommand(%q) = %#v, want %#v", tt.ref, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitCommand(%q)[%d] = %q, want %q (whole: %#v)",
						tt.ref, i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

// TestNewRefusesMetachars pins the construction-time refusal: a ref containing
// a shell metacharacter is refused before any Case runs, because there is no
// shell to give it a meaning and a silent literal pass-through is a
// command-injection door with a different label.
func TestNewRefusesMetachars(t *testing.T) {
	t.Parallel()
	metachars := []string{"|", "&", ";", "<", ">", "(", ")", "$", "`", "\\"}
	for _, m := range metachars {
		t.Run("metachar "+m, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Command: "cat " + m + "evil"})
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("New with %q: err = %v, want ErrInvalidInput", m, err)
			}
			if err.Error() == "" || !strings.Contains(err.Error(), "fix: ") {
				t.Errorf("New with %q: refusal carries no message or fix: %v", m, err)
			}
			if !strings.Contains(err.Error(), m) {
				t.Errorf("New with %q: refusal does not name the metacharacter: %v", m, err)
			}
		})
	}
}

// TestNewRefusesNUL pins that a NUL byte cannot hide in a ref.
func TestNewRefusesNUL(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Command: "cat\x00evil"})
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("New: err = %v, want ErrInvalidInput", err)
	}
}

// TestNewRefusesEmptyCommand pins the other construction-time refusal: an
// empty ref cannot name a process.
func TestNewRefusesEmptyCommand(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"", "   ", "\t\n"} {
		_, err := New(Options{Command: cmd})
		if !errors.Is(err, errs.ErrInvalidInput) {
			t.Errorf("New with %q: err = %v, want ErrInvalidInput", cmd, err)
		}
	}
}

// TestNewRefusesCommandNotOnPath pins the plan's edge-table row: a command
// that cannot be found is a construction-time refusal with the fix line,
// before any Case runs.
func TestNewRefusesCommandNotOnPath(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Command: "definitely-not-a-real-command-9f3a2"})
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("New: err = %v, want ErrInvalidInput", err)
	}
	if !strings.Contains(err.Error(), "fix: ") || !strings.Contains(err.Error(), "PATH") {
		t.Errorf("fix line does not point at PATH: %q", err)
	}
}

// TestNewRefusesMalformedGrants pins the env-grant grammar and the
// no-echo rule: a malformed grant is refused at construction, and the refusal
// never echoes the grant — a user who put a credential where a variable name
// belongs would otherwise see it in the error.
func TestNewRefusesMalformedGrants(t *testing.T) {
	t.Parallel()
	for _, grant := range []string{"KNO_TOKEN", "=value"} {
		t.Run(grant, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{Command: "sh testdata/good.sh", Env: []string{grant}})
			if !errors.Is(err, errs.ErrInvalidInput) {
				t.Fatalf("New with grant %q: err = %v, want ErrInvalidInput", grant, err)
			}
			if strings.Contains(err.Error(), grant) {
				t.Errorf("refusal echoed the grant %q; a credential must not reach stderr", grant)
			}
		})
	}
}

// TestNewAppliesDefaults pins the mirror-the-transport defaults: a zero
// timeout becomes DefaultTimeout and a zero cap becomes DefaultOutputCapBytes,
// so a hung script is bounded even by a user who never thought about either.
func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()
	a, err := New(Options{Command: "sh testdata/good.sh"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.timeout != DefaultTimeout {
		t.Errorf("timeout = %s, want the default %s", a.timeout, DefaultTimeout)
	}
	if a.outCap != DefaultOutputCapBytes {
		t.Errorf("outCap = %d, want the default %d", a.outCap, DefaultOutputCapBytes)
	}
}

// TestNewRefusesNegativeTimeout pins that a negative timeout cannot mean
// "bounded".
func TestNewRefusesNegativeTimeout(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Command: "sh testdata/good.sh", Timeout: -time.Second})
	if !errors.Is(err, errs.ErrInvalidInput) {
		t.Fatalf("New: err = %v, want ErrInvalidInput", err)
	}
}

// TestBuildEnvAllowlist pins the exact child environment: PATH, HOME, TMPDIR
// when the parent has them, plus the grants — nothing else, sorted.
func TestBuildEnvAllowlist(t *testing.T) {
	t.Setenv("KNO_EXEC_PARENT_SECRET", "s3cr3t")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/tester")
	// TMPDIR may be absent on some machines; the expected set is computed to
	// match, which is itself part of the pin.

	env, err := buildEnv([]string{"KNO_EXEC_GRANT=hello", "KNO_EXEC_GRANT2=world"})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	want := map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": "/home/tester",
		"KNO_EXEC_GRANT": "hello", "KNO_EXEC_GRANT2": "world",
	}
	if v, ok := os.LookupEnv("TMPDIR"); ok {
		want["TMPDIR"] = v
	}
	if _, present := got["KNO_EXEC_PARENT_SECRET"]; present {
		t.Error("a parent key reached the child environment; the allowlist is a boundary")
	}
	if len(got) != len(want) {
		t.Errorf("child env has %d keys %v, want exactly %d %v", len(got), keys(got), len(want), keys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("child env %s = %q, want %q", k, got[k], v)
		}
	}
	// The sort is part of the contract: the child's environment reads
	// identically on every run, so a diff over it is stable.
	for i := 1; i < len(env); i++ {
		if env[i-1] > env[i] {
			t.Errorf("child env is not sorted: %q before %q", env[i-1], env[i])
		}
	}
}

// TestBuildEnvGrantOverridesAllowlist pins last-wins: a grant whose name
// collides with the allowlist overrides it, which is how a user points a
// script at a different TMPDIR or PATH.
func TestBuildEnvGrantOverridesAllowlist(t *testing.T) {
	t.Setenv("TMPDIR", "/parent/tmp")
	env, err := buildEnv([]string{"TMPDIR=/granted/tmp"})
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == "TMPDIR" && v != "/granted/tmp" {
			t.Errorf("TMPDIR = %q, want the grant's %q", v, "/granted/tmp")
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
