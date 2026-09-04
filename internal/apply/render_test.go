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

// A rendering given none of the apply-time values leaves out what depends on them and
// says so, the same way an apply would (ADR 0006, ADR 0016).
func TestRenderWaitsForTheValuesItWasNotGiven(t *testing.T) {
	plan, err := New(Host{}, Options{}).Render(load(t, uplinkFixture), nil)
	if err != nil {
		t.Fatalf("rendering without the apply-time values failed: %v", err)
	}
	waiting := strings.Join(plan.Waiting, "\n")
	for _, want := range []string{"aftr.example.net", "/etc/regied/secrets/dhcpv6-duid"} {
		if !strings.Contains(waiting, want) {
			t.Errorf("the rendering does not say it waits for %s: %v", want, plan.Waiting)
		}
	}
	for _, change := range plan.Files {
		if strings.Contains(change.Path, "dslite") || change.Path == "/etc/systemd/network/50-regied-wan.network" {
			t.Errorf("%s was rendered without the value it depends on", change.Path)
		}
	}
	if !strings.Contains(renderReport(plan), "aftr.example.net") {
		t.Error("the printed rendering does not say what it waits for")
	}
}
