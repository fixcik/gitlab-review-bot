package meter

import "testing"

func TestRunFilterMatch(t *testing.T) {
	r := RunRecord{Kind: KindReview, ProjectPath: "g/a", MRIID: 3, Username: "alice", Models: []string{"opus", "haiku"}}
	alice, bob, none := "alice", "bob", ""
	for _, tc := range []struct {
		f    RunFilter
		want bool
	}{
		{RunFilter{}, true},
		{RunFilter{ProjectPath: "g/a"}, true}, {RunFilter{ProjectPath: "g/b"}, false},
		{RunFilter{Username: &alice}, true}, {RunFilter{Username: &bob}, false}, {RunFilter{Username: &none}, false},
		{RunFilter{Kind: KindReview}, true}, {RunFilter{Kind: KindChat}, false},
		{RunFilter{MRProjectPath: "g/a", MRIID: 3}, true}, {RunFilter{MRProjectPath: "g/b", MRIID: 3}, false}, {RunFilter{MRProjectPath: "g/a", MRIID: 4}, false},
		{RunFilter{Model: "haiku", HasModel: true}, true}, {RunFilter{Model: "sonnet", HasModel: true}, false}, {RunFilter{Model: "", HasModel: true}, false},
		{RunFilter{Model: "haiku"}, true}, // without HasModel the model is no condition
	} {
		if got := tc.f.Match(r); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.f, got, tc.want)
		}
	}
}
