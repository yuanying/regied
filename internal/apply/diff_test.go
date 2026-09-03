package apply

import (
	"strings"
	"testing"
)

func TestUnifiedDiffSaysNothingAboutTextThatDidNotChange(t *testing.T) {
	if got := unifiedDiff("a\nb\nc\n", "a\nb\nc\n"); got != "" {
		t.Errorf("two identical texts differ:\n%s", got)
	}
}

func TestUnifiedDiffShowsWhatMoved(t *testing.T) {
	before := "one\ntwo\nthree\n"
	after := "one\ntwo and a half\nthree\n"

	got := unifiedDiff(before, after)

	for _, want := range []string{"@@", "-two", "+two and a half", " one", " three"} {
		if !strings.Contains(got, want) {
			t.Errorf("the diff does not hold %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-one") || strings.Contains(got, "+one") {
		t.Errorf("a line that did not change is shown as a change:\n%s", got)
	}
}

func TestUnifiedDiffKeepsFarApartChangesInSeparateHunks(t *testing.T) {
	var before, after []string
	for i := range 40 {
		before = append(before, string(rune('a'+i%26))+"-line")
		after = append(after, string(rune('a'+i%26))+"-line")
	}
	after[0] = "first changed"
	after[39] = "last changed"

	got := unifiedDiff(strings.Join(before, "\n")+"\n", strings.Join(after, "\n")+"\n")

	if want, count := 2, strings.Count(got, "@@ "); count != want {
		t.Errorf("the diff has %d hunks, want %d:\n%s", count, want, got)
	}
}

func TestUnifiedDiffShowsAFileThatIsAllNew(t *testing.T) {
	got := unifiedDiff("", "hello\n")
	if !strings.Contains(got, "+hello") {
		t.Errorf("a new file's content is not shown:\n%s", got)
	}
}
