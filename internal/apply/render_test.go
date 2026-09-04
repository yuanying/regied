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

// A rendering is complete without the host. Nothing is left out of it and nothing has to
// be supplied on the command line for the ruleset to be whole, because the ruleset holds
// no value only a running host knows (ADR 0015, ADR 0006).
func TestRenderIsCompleteWithoutTheHost(t *testing.T) {
	engine := New(Host{}, Options{})

	plan, err := engine.Render(load(t, hostFixture+forwardResource), &Runtime{})
	if err != nil {
		t.Fatalf("rendering failed: %v", err)
	}

	if !strings.Contains(plan.Firewall.Ruleset, "ip daddr @uplink4_pppoe0") {
		t.Errorf("the hairpin translation is not in the rendering:\n%s", plan.Firewall.Ruleset)
	}
	// There is no host to read, so there is nothing to seed either. What a rendering
	// shows about the sets is that they are there and empty.
	if len(plan.Firewall.Elements) != 0 {
		t.Errorf("a rendering claims it would seed %v", plan.Firewall.Elements)
	}
}
