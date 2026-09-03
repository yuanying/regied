package apply

import (
	"strings"
	"testing"
)

// TestRenderReadsNothing holds `regied render` to what it is for: answering what a
// configuration means, anywhere, about any host (ADR 0006).
//
// The engine is built over a Host whose every part is nil, so anything that tried to
// read a file, run a command, resolve a name or look at a link would panic rather than
// quietly succeed.
func TestRenderReadsNothing(t *testing.T) {
	engine := New(Host{}, Options{})

	plan, err := engine.Render(load(t, hostFixture), &Runtime{})
	if err != nil {
		t.Fatalf("rendering failed: %v", err)
	}

	if _, ok := fileChangeFor(plan, "/etc/systemd/network/50-regied-wan.network"); !ok {
		t.Error("the rendering holds no networkd configuration")
	}
	if !strings.Contains(plan.Firewall.Ruleset, "table inet regied") {
		t.Error("the rendering holds no ruleset")
	}

	// There were no credentials to render the credentials file from, so it is reported
	// by path and mode and nothing else — which is all ADR 0003 would allow anyway.
	credentials, ok := fileChangeFor(plan, "/etc/regied/ppp/credentials/pppoe0.conf")
	if !ok {
		t.Fatal("the credentials file is not reported at all")
	}
	if !credentials.Withheld || credentials.Content != "" {
		t.Errorf("the credentials file was rendered without credentials: %+v", credentials)
	}
	if credentials.Mode != 0o600 {
		t.Errorf("the credentials file's mode is %o, want 600", credentials.Mode)
	}
}

func TestRenderShowsWhatDependsOnAValueThatWasNotSupplied(t *testing.T) {
	engine := New(Host{}, Options{})

	plan, err := engine.Render(load(t, hostFixture+forwardResource), &Runtime{})
	if err != nil {
		t.Fatalf("rendering failed: %v", err)
	}

	// Nothing said what the uplink is holding, so the hairpin translation cannot be
	// written and the ruleset says where it would have been.
	if !strings.Contains(plan.Firewall.Ruleset, "hairpin") {
		t.Errorf("the ruleset says nothing about the hairpin rules it left out:\n%s", plan.Firewall.Ruleset)
	}
}
