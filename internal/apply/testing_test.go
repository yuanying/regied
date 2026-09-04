package apply

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"slices"
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
`

// load builds a validated configuration out of the body of a document. A test whose own
// fixture does not validate is a broken test, not a failing engine, so both steps stop
// the test rather than fail it.
func load(t *testing.T, body string) *config.Config {
	t.Helper()
	document, err := config.Parse([]byte(documentHeader + body))
	if err != nil {
		t.Fatalf("the fixture does not parse:\n%v", err)
	}
	cfg, err := config.Validate(document, config.WithSecretFiles(everyFile{}))
	if err != nil {
		t.Fatalf("the fixture does not validate:\n%v", err)
	}
	return cfg
}

// everyFile stands in for the filesystem the credential and DUID files live on.
// Validation only checks that they are there; reading them is the engine's job and goes
// through fakeFiles.
type everyFile struct{}

func (everyFile) CheckSecretFile(string) error { return nil }

// fakeFiles is a filesystem in a map. Directories are implicit: a path is in the
// filesystem if it was written, and List answers from the paths that are.
type fakeFiles struct {
	files map[string]fakeFile

	// readErr and writeErr fail one path, so that a test can make an apply fail where
	// it wants it to.
	readErr  map[string]error
	writeErr map[string]error

	// writes, removes and dirs record what happened, in order.
	writes  []string
	removes []string
	dirs    []string
}

type fakeFile struct {
	data []byte
	mode fs.FileMode
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{
		files:    make(map[string]fakeFile),
		readErr:  make(map[string]error),
		writeErr: make(map[string]error),
	}
}

func (f *fakeFiles) put(path, content string, mode fs.FileMode) {
	f.files[path] = fakeFile{data: []byte(content), mode: mode}
}

func (f *fakeFiles) content(path string) (string, bool) {
	file, ok := f.files[path]
	return string(file.data), ok
}

func (f *fakeFiles) ReadFile(name string) ([]byte, fs.FileMode, error) {
	if err := f.readErr[name]; err != nil {
		return nil, 0, err
	}
	file, ok := f.files[name]
	if !ok {
		return nil, 0, fs.ErrNotExist
	}
	return slices.Clone(file.data), file.mode, nil
}

func (f *fakeFiles) WriteFile(name string, data []byte, mode fs.FileMode) error {
	if err := f.writeErr[name]; err != nil {
		return err
	}
	f.files[name] = fakeFile{data: slices.Clone(data), mode: mode}
	f.writes = append(f.writes, name)
	return nil
}

func (f *fakeFiles) MkdirAll(dir string, mode fs.FileMode) error {
	f.dirs = append(f.dirs, fmt.Sprintf("%s %04o", dir, mode))
	return nil
}

func (f *fakeFiles) Remove(name string) error {
	delete(f.files, name)
	f.removes = append(f.removes, name)
	return nil
}

func (f *fakeFiles) List(dir string) ([]string, error) {
	var names []string
	for name := range f.files {
		if path.Dir(name) == dir {
			names = append(names, path.Base(name))
		}
	}
	slices.Sort(names)
	return names, nil
}

// fakeRunner records the commands it was given and answers them from a table.
type fakeRunner struct {
	// output answers a command, keyed by its rendered form. A command with no entry
	// succeeds with no output.
	output map[string]string
	// fail makes one command fail, keyed by its rendered form.
	fail map[string]error

	// onRun is called before each command, so that a test can make the host change
	// under the engine the way a line coming up does.
	onRun func(Command)

	ran []Command
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{output: make(map[string]string), fail: make(map[string]error)}
}

func (r *fakeRunner) Run(_ context.Context, cmd Command) ([]byte, error) {
	if r.onRun != nil {
		r.onRun(cmd)
	}
	r.ran = append(r.ran, cmd)
	key := cmd.String()
	if err := r.fail[key]; err != nil {
		return nil, err
	}
	return []byte(r.output[key]), nil
}

// tablePresent makes the kernel answer that regied's table is there, and tableAbsent
// that it is not. Both are answers: a probe that fails is neither (ADR 0005).
func tablePresent(runner *fakeRunner) {
	runner.output["nft list tables"] = "table inet filter\ntable inet regied\n"
}

func tableAbsent(runner *fakeRunner) {
	runner.output["nft list tables"] = "table inet filter\n"
}

// setsHold makes the kernel answer that regied's table holds these uplink sets with these
// elements, in the JSON nft prints. A set not named is not in the table.
func setsHold(runner *fakeRunner, sets map[string][]string) {
	entries := []string{`{"metainfo":{"version":"1.1.1","release_name":"x","json_schema_version":1}}`,
		`{"table":{"family":"inet","name":"regied","handle":1}}`}
	names := make([]string, 0, len(sets))
	for name := range sets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		kind := "ipv4_addr"
		if strings.HasPrefix(name, "uplink6_") {
			kind = "ipv6_addr"
		}
		elements := ""
		if len(sets[name]) > 0 {
			elements = `,"elem":["` + strings.Join(sets[name], `","`) + `"]`
		}
		entries = append(entries, fmt.Sprintf(`{"set":{"family":"inet","name":"%s","table":"regied","type":"%s","handle":2%s}}`, name, kind, elements))
	}
	runner.output[listTableCommand().String()] = `{"nftables":[` + strings.Join(entries, ",") + `]}`
}

// commands is what was run, in order, as text.
func (r *fakeRunner) commands() []string {
	out := make([]string, len(r.ran))
	for i, cmd := range r.ran {
		out[i] = cmd.String()
	}
	return out
}

// fakeSysctl is a kernel switch table in a map.
type fakeSysctl struct {
	values map[string]string
	setErr map[string]error
	sets   []string
}

func newFakeSysctl() *fakeSysctl {
	return &fakeSysctl{values: make(map[string]string), setErr: make(map[string]error)}
}

func (s *fakeSysctl) Get(key string) (string, error) {
	value, ok := s.values[key]
	if !ok {
		return "", fs.ErrNotExist
	}
	return value, nil
}

func (s *fakeSysctl) Set(key, value string) error {
	if err := s.setErr[key]; err != nil {
		return err
	}
	s.values[key] = value
	s.sets = append(s.sets, key+"="+value)
	return nil
}

// fakeResolver answers name lookups from a table.
type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupHost(_ context.Context, name string) ([]netip.Addr, error) {
	addrs, ok := r[name]
	if !ok {
		return nil, fmt.Errorf("no such host %q", name)
	}
	return addrs, nil
}

// fakeLinks answers link addresses from a table. A link with no entry is not there.
type fakeLinks map[string][]netip.Addr

func (l fakeLinks) Addresses(ifname string) ([]netip.Addr, error) {
	addrs, ok := l[ifname]
	if !ok {
		return nil, ErrLinkNotFound
	}
	return addrs, nil
}

// testHost is a host every part of which is a fake.
func testHost() (Host, *fakeFiles, *fakeRunner) {
	files := newFakeFiles()
	runner := newFakeRunner()
	return Host{
		Files:    files,
		Runner:   runner,
		Resolver: fakeResolver{},
		Links:    fakeLinks{},
		Sysctl:   newFakeSysctl(),
	}, files, runner
}

func addrs(t *testing.T, values ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(values))
	for i, value := range values {
		addr, err := netip.ParseAddr(value)
		if err != nil {
			t.Fatalf("the fixture holds a bad address %q: %v", value, err)
		}
		out[i] = addr
	}
	return out
}

// requireErrorContaining fails the test unless err says what the caller expected.
func requireErrorContaining(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error saying %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("expected an error saying %q, got:\n%v", want, err)
	}
}

var errFake = errors.New("the fake was told to fail")
