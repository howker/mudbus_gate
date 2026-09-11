package buildinfo

import "testing"

func TestShortRevision(t *testing.T) {
	if got := (Info{Revision: "271032659467eb9ce69126504cca7b8f71d166fe"}).ShortRevision(); got != "271032659467" {
		t.Fatalf("ShortRevision() = %q", got)
	}
	if got := (Info{Revision: "abc"}).ShortRevision(); got != "abc" {
		t.Fatalf("short revision changed: %q", got)
	}
}

func TestCurrentAlwaysHasGoVersion(t *testing.T) {
	if got := Current().GoVersion; got == "" {
		t.Fatal("GoVersion must not be empty")
	}
}
