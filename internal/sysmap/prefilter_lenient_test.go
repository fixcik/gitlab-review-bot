package sysmap

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A .reviewbot.yaml the strict parser refuses (a removed key elsewhere)
// still has its sysmap.exclude honoured by the map scan.
func TestReadRepoConfigLenientExclude(t *testing.T) {
	dir := t.TempDir()
	cfg := "tools: [\"make lint\"]\nsysmap:\n  exclude: [\"legacy/**\", \"[bad\", \"/gen/\"]\n"
	if err := os.WriteFile(filepath.Join(dir, ".reviewbot.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readRepoConfig(dir).exclude
	if want := []string{"legacy/**", "gen"}; !reflect.DeepEqual(got, want) {
		t.Errorf("exclude = %q, want %q", got, want)
	}
}
