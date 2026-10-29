package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReactionsDefaults(t *testing.T) {
	r := Defaults().Review.Reactions
	want := map[string]string{"picked_up": "eyes", "done": "leftwards_arrow_with_hook", "failed": "x", "postponed": "hourglass_flowing_sand",
		"denied": "no_entry_sign", "thanks": "heart", "ack": "thumbsup", "learned": "brain", "clean_review": "tada"}
	if !r.Enabled {
		t.Fatal("reactions off by default")
	}
	for ev, name := range want {
		if got := r.Emoji(ev); got != name {
			t.Errorf("%s = %q, want %q", ev, got, name)
		}
	}
	if r.Emoji("nope") != "" {
		t.Error("unknown event")
	}
	if errs := r.validate(); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	r.Enabled = false
	if r.Emoji("done") != "" {
		t.Error("disabled reactions still give an emoji")
	}
}

func TestReactionsYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "review:\n  reactions:\n    done: \"\"\n    thanks: pray\n    ignore_users: [renovate-bot]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readService(p)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Review.Reactions
	if !r.Enabled || r.Emoji("done") != "" || r.Emoji("thanks") != "pray" || r.Emoji("picked_up") != "eyes" {
		t.Errorf("parsed %+v", r)
	}
	if !r.Ignored("Renovate-Bot") || r.Ignored("alice") {
		t.Error("ignore_users")
	}
}

func TestReactionsValidate(t *testing.T) {
	ok := defaultReactions()
	ok.Ack, ok.Done, ok.IgnoreUsers = "+1", "", []string{"@renovate-bot", "dependabot"}
	if errs := ok.validate(); len(errs) > 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	for _, tc := range []struct {
		name string
		edit func(*Reactions)
		want string
	}{
		{"upper case", func(r *Reactions) { r.PickedUp = "Eyes" }, "review.reactions.picked_up"},
		{"colons", func(r *Reactions) { r.Thanks = ":heart:" }, "review.reactions.thanks"},
		{"unicode emoji", func(r *Reactions) { r.CleanReview = "🎉" }, "review.reactions.clean_review"},
		{"space", func(r *Reactions) { r.Failed = "red circle" }, "review.reactions.failed"},
		{"too long", func(r *Reactions) { r.Learned = strings.Repeat("a", 65) }, "review.reactions.learned"},
		{"bad user", func(r *Reactions) { r.IgnoreUsers = []string{"a b"} }, "ignore_users"},
	} {
		r := defaultReactions()
		tc.edit(&r)
		if errs := strings.Join(r.validate(), "; "); !strings.Contains(errs, tc.want) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.want)
		}
	}
	svc := Defaults()
	svc.Review.Reactions.Postponed = "Hourglass"
	if err := svc.Validate(); err == nil || !strings.Contains(err.Error(), "review.reactions.postponed") {
		t.Errorf("Validate: %v", err)
	}
}

func TestReactionsIgnoredBuiltins(t *testing.T) {
	var r Reactions
	for _, u := range []string{"project_42_bot", "project_42_bot_0a1b2c3d", "group_7_bot", "support-bot", "ghost", "@alert-bot"} {
		if !r.Ignored(u) {
			t.Errorf("%s not ignored", u)
		}
	}
	for _, u := range []string{"", "alice", "project_bot", "robot"} {
		if r.Ignored(u) {
			t.Errorf("%s ignored", u)
		}
	}
}

func TestReactionsRepoCanOnlyDisable(t *testing.T) {
	svc := Defaults()
	repo, err := ParseRepo([]byte("reactions:\n  enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e := Merge(&svc, repo); e.Reactions.Enabled || e.Reactions.Emoji("done") != "" {
		t.Errorf("repo disable ignored: %+v", e.Reactions)
	}
	on, err := ParseRepo([]byte("reactions:\n  enabled: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	svc.Review.Reactions.Enabled = false
	if e := Merge(&svc, on); e.Reactions.Enabled {
		t.Error("repo turned reactions on against the service")
	}
	svc = Defaults()
	if e := Merge(&svc, nil); !e.Reactions.Enabled || e.Reactions.Emoji("thanks") != "heart" {
		t.Errorf("no repo config: %+v", e.Reactions)
	}
}
