package networkd

import (
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

// documentHeader is everything above spec.resources, so that a test can state the one
// kind it is about and nothing else.
const documentHeader = `apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata:
  name: test
spec:
  resources:
`

// load builds a validated configuration out of a resource list written as YAML. A test
// whose own fixture does not validate is a broken test, not a failing renderer, so both
// steps stop the test rather than fail it.
func load(t *testing.T, resources string) *config.Config {
	t.Helper()
	document, err := config.Parse([]byte(documentHeader + resources))
	if err != nil {
		t.Fatalf("the fixture does not parse:\n%v", err)
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(everyFile{}))
	if err != nil {
		t.Fatalf("the fixture does not validate:\n%v", err)
	}
	return cfg
}

// everyFile stands in for the filesystem the credential and DUID files live on. The
// renderer never reads them; validation only checks that they are there.
type everyFile struct{}

func (everyFile) CheckSecretFile(string) error { return nil }

// render renders a configuration that is expected to render.
func render(t *testing.T, cfg *config.Config, rt Runtime) *Output {
	t.Helper()
	out, err := Render(cfg, rt)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return out
}

// assertFile fails unless the output holds a file of that name with exactly that content.
func assertFile(t *testing.T, out *Output, name, want string) {
	t.Helper()
	for _, file := range out.Files {
		if file.Name != name {
			continue
		}
		if file.Content != want {
			t.Errorf("%s is\n%s\nwant\n%s", name, file.Content, want)
		}
		if got, want := file.Path(), Dir+"/"+name; got != want {
			t.Errorf("%s goes to %s, want %s", name, got, want)
		}
		if file.Mode != FileMode {
			t.Errorf("%s has mode %v, want %v", name, file.Mode, FileMode)
		}
		return
	}
	t.Errorf("no %s was rendered; got %s", name, strings.Join(names(out), ", "))
}

// assertNames fails unless exactly these files were rendered, in this order.
func assertNames(t *testing.T, out *Output, want ...string) {
	t.Helper()
	got := names(out)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rendered\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func names(out *Output) []string {
	got := make([]string, len(out.Files))
	for i, file := range out.Files {
		got[i] = file.Name
	}
	return got
}
