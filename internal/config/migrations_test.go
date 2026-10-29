package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestMigrationsConfig(t *testing.T) {
	d := Defaults().Review.Migrations
	if !d.Enabled || d.Dialect != "postgres" || d.MaxFiles != 50 || d.MaxUsages != 5 || len(d.Paths) != 0 {
		t.Fatalf("defaults %+v", d)
	}
	svc := Defaults()
	svc.Review.Migrations.Paths = []string{"schema/*.sql"}
	r, err := ParseRepo([]byte("migrations:\n  paths: [\"/db/changes/**/\"]\n  exclude: [legacy/**]\n  dialect: MySQL\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, r).Migrations
	if !e.Enabled || e.Dialect != "mysql" || !reflect.DeepEqual(e.Paths, []string{"schema/*.sql", "db/changes/**"}) ||
		!reflect.DeepEqual(e.Exclude, []string{"legacy/**"}) || e.MaxFiles != 50 {
		t.Errorf("merged %+v", e)
	}
	if e := Merge(&svc, nil).Migrations; !e.Enabled || e.Dialect != "postgres" {
		t.Errorf("no repo config %+v", e)
	}
	off, _ := ParseRepo([]byte("migrations:\n  enabled: false\n"))
	if Merge(&svc, off).Migrations.Enabled {
		t.Error("repo must be able to turn it off")
	}
	svc.Review.Migrations.Enabled = false
	on, _ := ParseRepo([]byte("migrations:\n  enabled: true\n"))
	if Merge(&svc, on).Migrations.Enabled {
		t.Error("the service switch wins")
	}
	for _, bad := range []string{"migrations:\n  dialect: oracle\n", "migrations:\n  paths: [\"[\"]\n"} {
		if _, err := ParseRepo([]byte(bad)); err == nil {
			t.Errorf("%q must fail", bad)
		}
	}
	svc = Defaults()
	svc.Review.Migrations = Migrations{Dialect: "sqlite", MaxFiles: 0, MaxUsages: 100, Exclude: []string{"["}}
	errs := strings.Join(svc.Review.Migrations.validate(), "; ")
	for _, want := range []string{"dialect", "max_files", "max_usages", "review.migrations.exclude"} {
		if !strings.Contains(errs, want) {
			t.Errorf("validation lacks %q: %s", want, errs)
		}
	}
}
