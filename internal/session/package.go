//go:build linux

package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/problem"
)

var (
	// ErrPackageMissing: no private package is stored under the hash.
	ErrPackageMissing = errors.New("session: the problem package is not stored")
	// ErrPackageCorrupt: the stored package does not match its hash.
	ErrPackageCorrupt = errors.New("session: the stored problem package is damaged")
)

const (
	packagesDir    = "packages"
	definitionFile = "definition.json"
	packageFiles   = "files"
)

var hashPattern = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// PackageDir is where the private package with this content hash lives:
// <stateDir>/packages/<hex>. The hash must be a problem.Hash value.
func PackageDir(stateDir, hash string) (string, error) {
	m := hashPattern.FindStringSubmatch(hash)
	if m == nil {
		return "", fmt.Errorf("session: %q is not a package hash", hash)
	}
	return filepath.Join(stateDir, packagesDir, m[1]), nil
}

// StorePackage keeps the complete, unrendered problem package (definition and
// every package file: setup, solution, cases, explanation) under its content
// hash so that later edits to the bank cannot change a session already
// frozen from it, and so that the private content stays in the management
// area instead of the learner's directory. It returns the hash.
//
// A package that is already stored is verified, not rewritten; content
// addressing makes a second copy redundant. The write is staged in a
// temporary directory and renamed, so a crash leaves no half package under
// the final name. Files are private (0600/0700).
func StorePackage(stateDir string, p model.Problem) (string, error) {
	hash, err := problem.Hash(p)
	if err != nil {
		return "", err
	}
	dest, err := PackageDir(stateDir, hash)
	if err != nil {
		return "", err
	}
	if _, err := LoadPackage(stateDir, hash); err == nil {
		return hash, nil
	} else if !errors.Is(err, ErrPackageMissing) {
		return "", err
	}

	root := filepath.Dir(dest)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("session: create %s: %w", root, err)
	}
	var tag [6]byte
	if _, err := rand.Read(tag[:]); err != nil {
		return "", fmt.Errorf("session: stage package: %w", err)
	}
	stage := filepath.Join(root, ".stage-"+hex.EncodeToString(tag[:]))
	if err := os.Mkdir(stage, 0o700); err != nil {
		return "", fmt.Errorf("session: stage package: %w", err)
	}
	defer os.RemoveAll(stage) // gone after the rename; cleans up after a failure

	def, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("session: encode package %s: %w", p.ID, err)
	}
	if err := writeFileSync(filepath.Join(stage, definitionFile), def); err != nil {
		return "", err
	}
	files := filepath.Join(stage, packageFiles)
	if err := os.Mkdir(files, 0o700); err != nil {
		return "", fmt.Errorf("session: stage package: %w", err)
	}
	for name, content := range p.Files {
		target, err := problem.ResolveAsset(files, name)
		if err != nil {
			return "", fmt.Errorf("session: package file %q: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return "", fmt.Errorf("session: stage package: %w", err)
		}
		if err := writeFileSync(target, content); err != nil {
			return "", err
		}
	}
	if err := os.Rename(stage, dest); err != nil {
		// Another writer stored the same content first: that is fine if it
		// verifies.
		if _, verr := LoadPackage(stateDir, hash); verr == nil {
			return hash, nil
		}
		return "", fmt.Errorf("session: store package %s: %w", p.ID, err)
	}
	return hash, nil
}

func writeFileSync(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("session: write %s: %w", path, err)
	}
	_, werr := f.Write(content)
	if serr := f.Sync(); werr == nil {
		werr = serr
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("session: write %s: %w", path, werr)
	}
	return nil
}

// LoadPackage returns the stored package with this content hash, including
// its private files, after proving that it still hashes to the key
// (ErrPackageCorrupt otherwise). ErrPackageMissing means nothing is stored
// under the hash, for example because the state directory was cleaned.
func LoadPackage(stateDir, hash string) (model.Problem, error) {
	dir, err := PackageDir(stateDir, hash)
	if err != nil {
		return model.Problem{}, err
	}
	def, err := os.ReadFile(filepath.Join(dir, definitionFile))
	if errors.Is(err, fs.ErrNotExist) {
		return model.Problem{}, fmt.Errorf("%w: %s", ErrPackageMissing, hash)
	}
	if err != nil {
		return model.Problem{}, fmt.Errorf("%w: %s: %w", ErrPackageCorrupt, hash, err)
	}
	var p model.Problem
	if err := json.Unmarshal(def, &p); err != nil {
		return model.Problem{}, fmt.Errorf("%w: %s: %w", ErrPackageCorrupt, hash, err)
	}
	p.Files = map[string][]byte{}
	root := filepath.Join(dir, packageFiles)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		p.Files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		return model.Problem{}, fmt.Errorf("%w: %s: %w", ErrPackageCorrupt, hash, err)
	}
	if got, err := problem.Hash(p); err != nil || got != hash {
		return model.Problem{}, fmt.Errorf("%w: %s hashes to %s", ErrPackageCorrupt, hash, strings.TrimSpace(got))
	}
	return p, nil
}
