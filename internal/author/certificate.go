package author

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"maps"
	"slices"

	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/store"
)

// CertificateMatches reports whether record certifies a problem served now:
// the validation passed AND it was made for exactly this problem content
// hash, grader version and environment hash (spec 7.1). A change to any of
// the three makes the old certificate stale; the problem must be validated
// again before it is served. Empty values never match.
func CertificateMatches(record model.ValidationRecord, problemHash, graderVersion, environmentHash string) bool {
	return record.Passed &&
		problemHash != "" && graderVersion != "" && environmentHash != "" &&
		record.ProblemHash == problemHash &&
		record.GraderVersion == graderVersion &&
		record.EnvironmentHash == environmentHash
}

// CertificateStore is the part of the store CurrentCertificate needs.
type CertificateStore interface {
	GetValidation(ctx context.Context, problemHash, graderVersion, environmentHash string) (model.ValidationRecord, error)
}

// CurrentCertificate returns the newest stored validation of the problem for
// this exact (problem hash, grader version, environment hash) and whether it
// certifies the problem. A missing record, a record of another triple (the
// store is keyed by the triple, so a changed component finds nothing) and a
// failed validation are all "not certified", not errors; only a store
// failure is an error. Sessions use it to refuse a problem that has no valid
// certificate.
func CurrentCertificate(ctx context.Context, s CertificateStore, problemHash, graderVersion, environmentHash string) (model.ValidationRecord, bool, error) {
	rec, err := s.GetValidation(ctx, problemHash, graderVersion, environmentHash)
	if errors.Is(err, store.ErrNotFound) {
		return model.ValidationRecord{}, false, nil
	}
	if err != nil {
		return model.ValidationRecord{}, false, err
	}
	return rec, CertificateMatches(rec, problemHash, graderVersion, environmentHash), nil
}

// EnvironmentHash is the certificate's environment component: SHA-256 over
// the environment's version manifest (EnvironmentManifest), independent of
// map order, with every key and value length-prefixed so entries cannot be
// shifted into each other. Any added, removed or changed entry changes it.
// It deliberately does not identify one environment instance: a rebuilt
// environment with the same versions keeps its certificates.
func EnvironmentHash(manifest map[string]string) string {
	h := sha256.New()
	chunk := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	h.Write([]byte("junkloud-environment-v1\x00"))
	for _, k := range slices.Sorted(maps.Keys(manifest)) {
		chunk(k)
		chunk(manifest[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
