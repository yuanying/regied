package nftables_test

import (
	"flag"
	"os"
	"testing"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/nftables"
)

var update = flag.Bool("update", false, "rewrite the golden files from what the renderer produces")

const goldenPath = "testdata/example.nft"

// config/example.yaml is the worked example docs/spec/ refers to. What it renders to is
// kept in the tree so that a change to the ruleset has to be read and agreed to rather
// than noticed later on a host. The apply engine and the netns tests take their
// expectation from here.
func TestExampleGolden(t *testing.T) {
	cfg, err := config.Load("../../../config/example.yaml", config.WithSecretFiles(anySecret{}))
	if err != nil {
		t.Fatalf("config/example.yaml does not validate:\n%v", err)
	}

	ruleset, err := nftables.Render(cfg)
	if err != nil {
		t.Fatalf("config/example.yaml does not render: %v", err)
	}
	got := ruleset.String()

	if *update {
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("cannot write %s: %v", goldenPath, err)
		}
		t.Logf("wrote %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("cannot read %s: %v", goldenPath, err)
	}
	if got != string(want) {
		t.Errorf("config/example.yaml renders differently from %s.\n"+
			"Read the difference before accepting it; run `go test ./internal/render/nftables -update` to take it.\n"+
			"got:\n%s", goldenPath, got)
	}
}
