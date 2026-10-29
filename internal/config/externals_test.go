package config

import (
	"strings"
	"testing"
)

// review.sysmap.externals: calls from these files of a repository go to an
// external system whose address the code does not show.
func TestSysmapExternals(t *testing.T) {
	if d := Defaults(); len(d.Review.Sysmap.Externals) != 0 {
		t.Fatal("externals must be empty by default")
	}
	s := mutSvc()
	s.GitLab.Groups = []string{"team"}
	s.Review.Sysmap.Externals = []SysmapExternal{{Name: " Bitrix24 ", Repo: "/team/shop-backend/", Paths: []string{"components/bitrix/**", " commands/BitrixAuditController.php "}, Host: "bitrix24.ru"}}
	wantValid(t, "externals", s)
	if n := s.Review.Sysmap.Externals[0].Normalized(); n.Name != "Bitrix24" || n.Repo != "team/shop-backend" || n.Paths[1] != "commands/BitrixAuditController.php" {
		t.Errorf("normalized %+v", n)
	}
	for _, c := range []struct {
		ext  SysmapExternal
		want string
	}{
		{SysmapExternal{Name: "", Repo: "team/b", Paths: []string{"a/**"}}, "name is empty"},
		{SysmapExternal{Name: "X", Repo: "b", Paths: []string{"a/**"}}, `"b" is not a full project path`},
		{SysmapExternal{Name: "X", Repo: "other/b", Paths: []string{"a/**"}}, `"other/b" is not in gitlab.groups`},
		{SysmapExternal{Name: "X", Repo: "team/b"}, "paths: at least one glob"},
		{SysmapExternal{Name: "X", Repo: "team/b", Paths: []string{"a/["}}, `paths: invalid glob "a/["`},
		{SysmapExternal{Name: "X", Repo: "team/b", Paths: []string{"a/**"}, Host: "not a host"}, `host "not a host"`},
		{SysmapExternal{Name: strings.Repeat("x", 81), Repo: "team/b", Paths: []string{"a/**"}}, "name: at most 80 characters"},
	} {
		s := mutSvc()
		s.GitLab.Groups = []string{"team"}
		s.Review.Sysmap.Externals = []SysmapExternal{c.ext}
		wantInvalid(t, c.want, s, "review.sysmap.externals[0]: ")
		wantInvalid(t, c.want, s, c.want)
	}
	many := mutSvc()
	for i := 0; i <= MaxSysmapExternals; i++ {
		many.Review.Sysmap.Externals = append(many.Review.Sysmap.Externals, SysmapExternal{Name: "X", Repo: "g/b", Paths: []string{"a"}})
	}
	wantInvalid(t, "too many", many, "review.sysmap.externals: at most")
	if _, err := ParseRepo([]byte("sysmap:\n  externals: [{name: X, repo: g/a, paths: [a]}]\n")); err == nil || !strings.Contains(err.Error(), "sysmap.externals: unknown key") {
		t.Errorf("repo externals: %v", err)
	}
}
