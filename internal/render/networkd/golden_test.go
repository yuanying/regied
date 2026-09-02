package networkd

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the renderer produced")

const goldenDir = "testdata/example"

// The DUID and the resolved AFTR address the apply engine would have read and looked up
// before rendering the example. Both are documentation values.
const exampleDUID = "00:03:00:01:00:00:5e:00:53:01"

var exampleAFTR = netip.MustParseAddr("2001:db8:ffff::64")

// config/example.yaml is the worked example docs/spec/ refers to, and this is what it
// renders into. The whole output is held here rather than described, because the apply
// engine and the integration tests take this as the shape they have to handle: a change
// to any of it should be seen in a diff and argued for, not discovered later.
func TestExampleGolden(t *testing.T) {
	cfg, err := config.Load("../../../config/example.yaml", config.WithSecretFiles(everyFile{}))
	if err != nil {
		t.Fatalf("config/example.yaml does not validate:\n%v", err)
	}

	out := render(t, cfg, Runtime{
		AFTRAddresses: map[string]netip.Addr{"dslite": exampleAFTR},
		DUIDs:         map[string]string{"/etc/regied/secrets/dhcpv6-duid": exampleDUID},
	})

	if *update {
		writeGolden(t, out)
	}

	filesDir := filepath.Join(goldenDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		t.Fatalf("cannot read the golden files: %v", err)
	}
	want := make([]string, len(entries))
	for i, entry := range entries {
		want[i] = entry.Name()
	}
	assertNames(t, out, want...)

	for _, file := range out.Files {
		golden, err := os.ReadFile(filepath.Join(filesDir, file.Name))
		if err != nil {
			t.Errorf("cannot read the golden %s: %v", file.Name, err)
			continue
		}
		assertFile(t, out, file.Name, string(golden))
	}

	golden, err := os.ReadFile(filepath.Join(goldenDir, "warnings.txt"))
	if err != nil {
		t.Fatalf("cannot read the golden warnings: %v", err)
	}
	if got := warningsText(out); got != string(golden) {
		t.Errorf("the warnings are\n%s\nwant\n%s", got, golden)
	}
}

func warningsText(out *Output) string {
	var b strings.Builder
	for _, warning := range out.Warnings {
		b.WriteString(warning)
		b.WriteString("\n")
	}
	return b.String()
}

func writeGolden(t *testing.T, out *Output) {
	t.Helper()
	filesDir := filepath.Join(goldenDir, "files")
	if err := os.RemoveAll(filesDir); err != nil {
		t.Fatalf("cannot clear the golden files: %v", err)
	}
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatalf("cannot create the golden directory: %v", err)
	}
	for _, file := range out.Files {
		if err := os.WriteFile(filepath.Join(filesDir, file.Name), []byte(file.Content), 0o644); err != nil {
			t.Fatalf("cannot write the golden %s: %v", file.Name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(goldenDir, "warnings.txt"), []byte(warningsText(out)), 0o644); err != nil {
		t.Fatalf("cannot write the golden warnings: %v", err)
	}
}
