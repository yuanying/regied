package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
	"github.com/yuanying/regied/internal/render/dnsmasq"
	"github.com/yuanying/regied/internal/render/pppd"
)

// TestRenderedFilesCarryTheOwnershipMarker holds this package's copy of the marker
// against the renderers'. Reclaiming a file depends on recognising it, so the day the
// two drift apart is the day an apply stops taking away what it left behind (ADR 0009).
func TestRenderedFilesCarryTheOwnershipMarker(t *testing.T) {
	cfg := load(t, hostFixture)

	sessions := pppd.Render(cfg)
	if len(sessions.Sessions) == 0 {
		t.Fatal("the fixture declares no session")
	}
	for _, session := range sessions.Sessions {
		if !strings.HasPrefix(session.Peer.Content, ownershipMarker) {
			t.Errorf("the peer file of %s does not start with the marker this package reclaims by", session.Name)
		}
		file, err := session.Credentials.Render(pppd.Credentials{UserID: "u", Password: "p"})
		if err != nil {
			t.Fatalf("rendering the credentials file failed: %v", err)
		}
		if !strings.HasPrefix(file.Content, ownershipMarker) {
			t.Errorf("the credentials file of %s does not start with the marker", session.Name)
		}
	}

	names := dnsmasq.Render(cfg)
	for _, file := range names.Files() {
		if !strings.HasPrefix(file.Content, ownershipMarker) {
			t.Errorf("%s does not start with the marker", file.Path)
		}
	}
}

// TestTheExampleCanBeApplied is the task's third completion condition, from the engine's
// side: the worked example plans without a host being touched, and everything the
// operator was promised is in the plan.
//
// It asserts the shape rather than the text. The example belongs to the schema, and a
// change to it should show up in the renderers' golden files, not here.
func TestTheExampleCanBeApplied(t *testing.T) {
	host, files, runner := testHost()
	for _, path := range []string{
		"/etc/regied/secrets/dhcpv6-duid",
		"/etc/regied/secrets/pppoe-user-id",
		"/etc/regied/secrets/pppoe-password",
	} {
		files.put(path, "00:03:00:01:00:00:5e:00:53:01\n", 0o600)
	}
	host.Resolver = fakeResolver{"aftr.example.net": addrs(t, "2001:db8:53::1")}
	tableAbsent(runner)

	cfg, err := config.Load("../../config/example.yaml", config.WithSecretFiles(everyFile{}))
	if err != nil {
		t.Fatalf("config/example.yaml does not validate:\n%v", err)
	}

	plan, err := New(host, Options{}).Plan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("the example cannot be planned:\n%v", err)
	}
	if plan.Empty() {
		t.Fatal("planning the example against an untouched host changes nothing")
	}
	if !strings.Contains(plan.Firewall.Ruleset, "table inet regied") {
		t.Error("the plan holds no nftables ruleset")
	}
	if change, ok := fileChangeFor(plan, "/etc/regied/dnsmasq/dnsmasq.conf"); !ok || change.Kind != ChangeCreate {
		t.Error("the plan holds no dnsmasq configuration")
	}
	// The routing an operator asked to see is in the networkd files: the policies'
	// tables and the rules that select them.
	var routes bool
	for _, change := range plan.Files {
		if strings.Contains(change.Content, "[RoutingPolicyRule]") {
			routes = true
		}
	}
	if !routes {
		t.Error("the plan holds no routing policy rules")
	}
	if len(files.writes) != 0 {
		t.Errorf("planning wrote to the host: %v", files.writes)
	}
}
