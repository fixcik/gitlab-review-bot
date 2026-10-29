package telemetry

import "testing"

func TestObserveSkip(t *testing.T) {
	m := New("test")
	m.ObserveSkip(SkipTooBig)
	m.ObserveSkip(SkipTooBig)
	has(t, scrape(t, m), `reviewbot_skipped_total{reason="too_big"} 2`)
	var nilM *Metrics
	nilM.ObserveSkip(SkipTooBig)
}

func TestObserveSkipRepoConfig(t *testing.T) {
	m := New("test")
	m.ObserveSkip("no_repo_config")
	m.ObserveSkip("no_repo_config")
	m.ObserveSkip("repo_config_invalid")
	has(t, scrape(t, m),
		`reviewbot_skipped_total{reason="no_repo_config"} 2`,
		`reviewbot_skipped_total{reason="repo_config_invalid"} 1`,
	)
}

func TestObserveGitLabSaved(t *testing.T) {
	m := New("test")
	m.ObserveGitLabSaved("list_discussions")
	m.ObserveGitLabSaved("list_discussions")
	m.ObserveGitLabSaved("get_mr_diffs")
	has(t, scrape(t, m),
		`reviewbot_gitlab_reads_saved_total{op="list_discussions"} 2`,
		`reviewbot_gitlab_reads_saved_total{op="get_mr_diffs"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveGitLabSaved("get_mr")
}

func TestObserveRouteAndTrivialSkip(t *testing.T) {
	m := New("test")
	m.ObserveRoute("light")
	m.ObserveRoute("normal")
	m.ObserveRoute("normal")
	m.ObserveSkip(SkipTrivialIncrement)
	has(t, scrape(t, m),
		`reviewbot_review_route_total{route="light"} 1`,
		`reviewbot_review_route_total{route="normal"} 2`,
		`reviewbot_skipped_total{reason="trivial_increment"} 1`,
	)
	var nilM *Metrics
	nilM.ObserveRoute("light")
}
