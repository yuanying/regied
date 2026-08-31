package config

import (
	"errors"
	"io"
	"os"
)

// Why a secret file cannot be used. A configuration naming one that is not there is
// refused rather than warned about: bringing an uplink up without authentication is not
// a degraded success (ADR 0003).
var (
	ErrSecretFileMissing    = errors.New("file is missing")
	ErrSecretFileEmpty      = errors.New("file is empty")
	ErrSecretFileUnreadable = errors.New("file cannot be read")
)

// FileChecker answers whether the file a credential — or the DUID — was put in can be
// used. It is an interface so that validation can be tested without a filesystem, and so
// that a caller validating a configuration for another host can say what exists there.
type FileChecker interface {
	// CheckSecretFile returns nil if the file at path exists, can be read, and is not
	// empty. It never returns the contents: nothing in validation needs them.
	CheckSecretFile(path string) error
}

// OSFiles checks the filesystem this process is running on. It is the default.
type OSFiles struct{}

func (OSFiles) CheckSecretFile(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrSecretFileMissing
	case err != nil:
		return ErrSecretFileUnreadable
	case info.IsDir():
		return ErrSecretFileUnreadable
	}
	file, err := os.Open(path)
	if err != nil {
		return ErrSecretFileUnreadable
	}
	defer file.Close()

	var first [1]byte
	n, err := file.Read(first[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrSecretFileUnreadable
	}
	if n == 0 {
		return ErrSecretFileEmpty
	}
	return nil
}

// Option changes how a document is validated.
type Option func(*options)

type options struct {
	files FileChecker
}

// WithSecretFiles says where to look for the files credentials and the DUID were put in.
func WithSecretFiles(files FileChecker) Option {
	return func(o *options) { o.files = files }
}

func resolveOptions(opts []Option) options {
	resolved := options{files: OSFiles{}}
	for _, opt := range opts {
		opt(&resolved)
	}
	return resolved
}
