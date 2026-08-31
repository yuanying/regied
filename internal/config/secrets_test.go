package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yuanying/regied/internal/config"
)

// The default checker is the one that runs on the host, so it is worth knowing that it
// tells the three cases apart rather than lumping them into "cannot use".
func TestOSFilesCheckSecretFile(t *testing.T) {
	dir := t.TempDir()

	present := filepath.Join(dir, "password")
	if err := os.WriteFile(present, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want error
	}{
		{"a file with something in it", present, nil},
		{"a file with nothing in it", empty, config.ErrSecretFileEmpty},
		{"a path that is not there", filepath.Join(dir, "absent"), config.ErrSecretFileMissing},
		{"a directory", dir, config.ErrSecretFileUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.OSFiles{}.CheckSecretFile(tc.path)
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}
