package config

import "testing"

func TestDiagramSwitch(t *testing.T) {
	svc := Defaults()
	if !svc.Review.Diagram || !Merge(&svc, nil).Diagram {
		t.Fatal("diagram must be on by default")
	}
	off, err := ParseRepo([]byte("diagram: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Merge(&svc, off).Diagram {
		t.Fatal("repo diagram: false must turn it off")
	}
	on, _ := ParseRepo([]byte("diagram: true\n"))
	if !Merge(&svc, on).Diagram {
		t.Fatal("repo diagram: true keeps it on")
	}
	svc.Review.Diagram = false
	if Merge(&svc, on).Diagram {
		t.Fatal("repo cannot turn the diagram on when the service has it off")
	}
}
