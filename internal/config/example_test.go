package config_test

import (
	"testing"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
	"github.com/yuanying/regied/internal/config"
)

// config/example.yaml is the worked example docs/spec/ refers to. It has to go on being
// readable by the code the spec describes, or the two have drifted apart.
func TestExampleConfiguration(t *testing.T) {
	cfg, err := config.Load("../../config/example.yaml", config.WithSecretFiles(exampleSecrets{}))
	if err != nil {
		t.Fatalf("config/example.yaml does not validate:\n%v", err)
	}
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Errorf("config/example.yaml warns:\n%s", warnings)
	}

	// Every kind the example uses came back as its own type.
	for _, kind := range []v1alpha1.ResourceKind{
		v1alpha1.KindInterface, v1alpha1.KindPPPoESession, v1alpha1.KindDSLiteTunnel,
		v1alpha1.KindEgressRoutePolicy, v1alpha1.KindIPAddressSet, v1alpha1.KindFirewallZone,
		v1alpha1.KindFirewallPolicy, v1alpha1.KindSourceNAT, v1alpha1.KindPortForward,
		v1alpha1.KindDHCPServer, v1alpha1.KindDNSForwarder,
	} {
		if len(cfg.ByKind(kind)) == 0 {
			t.Errorf("no %s came back from the example", kind)
		}
	}

	// The two policies got a table and a mark each, and they differ.
	first, ok := cfg.PolicyRouting("upper-half-via-pppoe")
	if !ok {
		t.Fatal("no routing derived for upper-half-via-pppoe")
	}
	second, ok := cfg.PolicyRouting("rest-via-dslite")
	if !ok {
		t.Fatal("no routing derived for rest-via-dslite")
	}
	if first.Table == second.Table || first.Mark == second.Mark {
		t.Errorf("the two policies share a number: %+v %+v", first, second)
	}

	// The typed view the renderers will use.
	interfaces := config.ResourcesOf[*v1alpha1.InterfaceSpec](cfg)
	if len(interfaces) != 2 {
		t.Fatalf("got %d interfaces, want 2", len(interfaces))
	}
	if interfaces[0].Name != "wan" || interfaces[0].Spec.Ifname != "eth0" {
		t.Errorf("first interface: %+v", interfaces[0])
	}
	if bridge := interfaces[1].Spec.Bridge; bridge == nil || len(bridge.Members) != 3 {
		t.Errorf("second interface is a bridge over three ports: %+v", interfaces[1].Spec.Bridge)
	}

	// The defaults the schema promises, for fields the example leaves out.
	sessions := config.ResourcesOf[*v1alpha1.PPPoESessionSpec](cfg)
	if len(sessions) != 1 {
		t.Fatalf("got %d PPPoE sessions, want 1", len(sessions))
	}
	if !sessions[0].Spec.PersistEnabled() {
		t.Error("persist defaults to true")
	}

	forwards := config.ResourcesOf[*v1alpha1.PortForwardSpec](cfg)
	for _, forward := range forwards {
		if !forward.Spec.HairpinEnabled() || !forward.Spec.OpenFirewallEnabled() {
			t.Errorf("%s: hairpin and openFirewall default to true", forward.Name)
		}
	}
	if got := forwards[2].Spec.TargetPorts().String(); got != "60000-60010" {
		t.Errorf("a target with no port keeps the range it listens on, got %s", got)
	}
}

// The example names files under /etc/regied/secrets/, which is not part of this project.
type exampleSecrets struct{}

func (exampleSecrets) CheckSecretFile(path string) error {
	switch path {
	case "/etc/regied/secrets/dhcpv6-duid",
		"/etc/regied/secrets/pppoe-user-id",
		"/etc/regied/secrets/pppoe-password":
		return nil
	}
	return config.ErrSecretFileMissing
}
