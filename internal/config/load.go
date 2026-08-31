package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/yuanying/regied/internal/apis/v1alpha1"
)

// ParseError is what Parse returns for a document that could not be read into the
// schema: bad YAML, a malformed value, or a field nobody declared.
type ParseError struct {
	Path     string // the file the document came from, when it came from one
	Messages []string
}

func (e *ParseError) Error() string {
	head := "cannot parse the configuration"
	if e.Path != "" {
		head = "cannot parse " + e.Path
	}
	return head + ":\n  " + strings.Join(e.Messages, "\n  ")
}

// Load reads one configuration file and returns the validated model.
func Load(path string, opts ...Option) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	document, err := Parse(data)
	if err != nil {
		var parseErr *ParseError
		if errors.As(err, &parseErr) {
			parseErr.Path = path
		}
		return nil, err
	}
	cfg, err := Validate(document, opts...)
	if err != nil {
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			invalid.Path = path
		}
		return nil, err
	}
	return cfg, nil
}

// Parse reads one document into the schema.
//
// Unknown fields are refused. A misspelt field that is silently dropped is the worst way
// this kind of configuration can break: the file is accepted, the setting is not there,
// and nothing says so until traffic goes somewhere it should not have.
func Parse(data []byte) (*v1alpha1.NetworkConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var document v1alpha1.NetworkConfig
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &ParseError{Messages: []string{"the file holds no document"}}
		}
		return nil, &ParseError{Messages: parseMessages(err)}
	}

	// One YAML document per host. A second one would otherwise be read past and thrown
	// away without a word.
	if err := decoder.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, &ParseError{Messages: []string{"a configuration file holds one document per host, and this one holds more"}}
	}

	if document.APIVersion != v1alpha1.APIVersion {
		return nil, &ParseError{Messages: []string{fmt.Sprintf("apiVersion is %q, want %q", document.APIVersion, v1alpha1.APIVersion)}}
	}
	if document.Kind != v1alpha1.DocumentKind {
		return nil, &ParseError{Messages: []string{fmt.Sprintf("kind is %q, want %q", document.Kind, v1alpha1.DocumentKind)}}
	}
	return &document, nil
}

// The decoder names the Go type a field was not found in. That is the wrong vocabulary
// for somebody reading their own configuration file, so the message is put back into the
// schema's terms.
var unknownField = regexp.MustCompile(`^(line \d+: )?field (\S+) not found in type .*$`)

func parseMessages(err error) []string {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return []string{strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	messages := make([]string, len(typeErr.Errors))
	for i, message := range typeErr.Errors {
		messages[i] = unknownField.ReplaceAllString(message, `${1}unknown field "${2}"`)
	}
	return messages
}
