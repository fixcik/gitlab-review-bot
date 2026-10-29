package config

import "testing"

// review.description_summary: off by default (the bot would write into
// other people's MR descriptions); a repository can turn it off where the
// service turned it on, like diagram.
func TestDescriptionSummary(t *testing.T) {
	svc := Defaults()
	if svc.Review.DescriptionSummary || Merge(&svc, nil).DescriptionSummary {
		t.Fatal("description_summary must be off by default")
	}
	if Merge(&svc, mustRepo(t, "description_summary: true\n")).DescriptionSummary {
		t.Error("a repository cannot turn it on alone")
	}
	svc.Review.DescriptionSummary = true
	if !Merge(&svc, nil).DescriptionSummary || !Merge(&svc, mustRepo(t, "description_summary: true\n")).DescriptionSummary {
		t.Error("service on")
	}
	if Merge(&svc, mustRepo(t, "description_summary: false\n")).DescriptionSummary {
		t.Error("the repository turns it off")
	}
}
