package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

// doc wraps a resources block in the document every test shares. The block is written at
// the indentation it has inside spec.resources.
func doc(resources string) []byte {
	return []byte(`apiVersion: net.unstable.cloud/v1alpha1
kind: NetworkConfig
metadata:
  name: test
spec:
  global:
    ipForwarding: true
  resources:
` + resources)
}

// secretFiles is a FileChecker standing in for the filesystem. A path it does not know
// is missing, which is what the real one reports too.
type secretFiles map[string]string

func (f secretFiles) CheckSecretFile(path string) error {
	content, ok := f[path]
	if !ok {
		return config.ErrSecretFileMissing
	}
	if content == "" {
		return config.ErrSecretFileEmpty
	}
	return nil
}

// anySecret accepts every path, for tests that are not about secrets.
type anySecret struct{}

func (anySecret) CheckSecretFile(string) error { return nil }

// assertProblems checks that every wanted fragment appears in a problem of the given
// severity, and that no unexpected problem of that severity was reported.
func assertProblems(t *testing.T, got config.Problems, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(got), len(want), got)
	}
	for _, fragment := range want {
		found := false
		for _, p := range got {
			if strings.Contains(p.String(), fragment) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem mentions %q; got:\n%s", fragment, got)
		}
	}
}

// asValidationError is errors.As, spelt out so that the cases read without the import.
func asValidationError(err error, target **config.ValidationError) bool {
	return errors.As(err, target)
}
