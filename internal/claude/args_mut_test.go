package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The read-only allowlist is passed through in full (not dropped by an
// off-by-one); a text-only run emits no allowlist at all.
func TestReadToolsAllowlist(t *testing.T) {
	args := strings.Join(buildClaudeArgs(core.RunRequest{Prompt: "x"}), " ")
	if !strings.Contains(args, "--allowedTools Read(//work/**) --disallowedTools") {
		t.Fatalf("args: %s", args)
	}
	if args := strings.Join(buildClaudeArgs(core.RunRequest{NoTools: true}), " "); strings.Contains(args, "--allowedTools") {
		t.Fatalf("a text-only run must not emit the flag: %s", args)
	}
}

// Exec env names: upper-case letters, '_' and (not first) digits, across the
// full ranges.
func TestValidExecEnvCharacterRanges(t *testing.T) {
	for _, kv := range []string{"A=1", "Z=1", "AZ_09=1", "X0=1", "X9=1", "_A=1"} {
		if !validExecEnv(kv) {
			t.Errorf("refused %q", kv)
		}
	}
	for _, kv := range []string{"0A=1", "9=1", "a=1", "A-B=1", "A B=1", "@=1", "[=1", "A/=1", "A:=1"} {
		if validExecEnv(kv) {
			t.Errorf("accepted %q", kv)
		}
	}
}

// An existing host directory whose path contains ':' or ',' would change the
// meaning of a docker -v spec (e.g. "dir:/work" mounts over the clone): it is
// never mounted, for the merge base, context repos and skills alike.
func TestMountPathsWithDockerSeparatorsDropped(t *testing.T) {
	base := t.TempDir()
	var bad []string
	for _, n := range []string{"snap:rw", "snap,ro"} {
		p := filepath.Join(base, n)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		bad = append(bad, p)
	}
	for i, p := range bad {
		if got := usableContextDirs([]core.ContextDir{{HostPath: p, Name: "g/p" + string(rune('a'+i))}}, nil); len(got) != 0 {
			t.Errorf("context dir %q accepted", p)
		}
		if got := usableSkillsDir(p, nil); got != "" {
			t.Errorf("skills dir %q accepted", p)
		}
		if got := usableBaseDir(p, "/work/clone", nil); got != "" {
			t.Errorf("base dir %q accepted", p)
		}
	}
	if m := skillsMounts(""); m != nil {
		t.Errorf("no skills dir must mean no mount: %+v", m)
	}
}
