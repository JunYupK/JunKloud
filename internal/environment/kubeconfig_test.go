//go:build linux

package environment

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JunYupK/JunKloud/internal/testutil"
)

// testPKI returns a PEM CA certificate plus a PEM client cert/key pair.
func testPKI(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	testutil.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	testutil.NoError(t, err)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	testutil.NoError(t, err)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caPEM, caPEM, keyPEM // the CA doubles as the client cert: shape is all that matters here
}

func kindInternalKubeconfig(t *testing.T, cluster string, extra string) []byte {
	t.Helper()
	ca, cert, key := testPKI(t)
	enc := func(b []byte) string { return b64(b) }
	return []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ` + enc(ca) + `
    server: https://` + cluster + `-control-plane:6443` + extra + `
  name: kind-` + cluster + `
contexts:
- context:
    cluster: kind-` + cluster + `
    user: kind-` + cluster + `
  name: kind-` + cluster + `
current-context: kind-` + cluster + `
users:
- name: kind-` + cluster + `
  user:
    client-certificate-data: ` + enc(cert) + `
    client-key-data: ` + enc(key) + `
`)
}

// TestIgnoreAmbientKubeconfig: the JunKloud kubeconfig is built from kind's
// output and read back only from the state directory. A decoy $KUBECONFIG and
// ~/.kube/config are neither read nor written, and TLS is always verified
// against the cluster CA.
func TestIgnoreAmbientKubeconfig(t *testing.T) {
	home := t.TempDir()
	decoyDir := filepath.Join(home, ".kube")
	testutil.NoError(t, os.MkdirAll(decoyDir, 0o700))
	decoy := []byte("apiVersion: v1\nkind: Config\ncurrent-context: decoy\nclusters:\n- name: decoy\n  cluster:\n    server: https://127.0.0.1:1\n    insecure-skip-tls-verify: true\ncontexts:\n- name: decoy\n  context: {cluster: decoy, user: decoy}\nusers:\n- name: decoy\n  user: {token: x}\n")
	decoyPath := filepath.Join(decoyDir, "config")
	testutil.NoError(t, os.WriteFile(decoyPath, decoy, 0o600))
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", decoyPath)

	dir := t.TempDir()
	m := New(dir, openTestStore(t, dir), WithRunner(&fakeRunner{}))
	internal := kindInternalKubeconfig(t, ClusterName("0123456789ab"), "")
	access, err := buildAccess(internal)
	testutil.NoError(t, err)
	endpoint, err := endpointFor(access.Server, "172.18.0.7")
	testutil.NoError(t, err)
	testutil.Equal(t, endpoint, "https://172.18.0.7:6443")
	testutil.NoError(t, writeKubeconfig(m.KubeconfigPath(), "junkloud-0123456789ab", endpoint, access))

	info, err := os.Stat(m.KubeconfigPath())
	testutil.NoError(t, err)
	testutil.Equal(t, info.Mode().Perm(), os.FileMode(0o600))

	got, err := loadAccess(m.KubeconfigPath(), "junkloud-0123456789ab")
	testutil.NoError(t, err)
	testutil.Equal(t, got.Server, endpoint)
	testutil.Equal(t, got.CAData, access.CAData)
	cfg := restConfig(got, endpoint)
	testutil.Equal(t, cfg.Host, endpoint)
	testutil.Equal(t, cfg.TLSClientConfig.Insecure, false)
	testutil.Equal(t, cfg.TLSClientConfig.CAData, access.CAData)
	testutil.Equal(t, cfg.BearerToken, "")

	// The decoy context is not in our file, so it cannot be selected.
	_, err = loadAccess(m.KubeconfigPath(), "decoy")
	if err == nil {
		t.Fatal("decoy context resolved from the JunKloud kubeconfig")
	}
	after, err := os.ReadFile(decoyPath)
	testutil.NoError(t, err)
	testutil.Equal(t, bytes.Equal(after, decoy), true)
	if strings.Contains(string(mustRead(t, m.KubeconfigPath())), "127.0.0.1:1") {
		t.Fatal("decoy server leaked into the JunKloud kubeconfig")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	testutil.NoError(t, err)
	return b
}

func TestBuildAccessRejectsUnverifiedTLS(t *testing.T) {
	insecure := kindInternalKubeconfig(t, "x", "\n    insecure-skip-tls-verify: true")
	_, err := buildAccess(insecure)
	if err == nil {
		t.Fatal("kubeconfig without TLS verification accepted")
	}
	noCA := []byte(strings.Replace(string(kindInternalKubeconfig(t, "x", "")), "certificate-authority-data", "x-ignored", 1))
	_, err = buildAccess(noCA)
	if err == nil {
		t.Fatal("kubeconfig without a CA accepted")
	}
}

func TestCAHash(t *testing.T) {
	ca1, _, _ := testPKI(t)
	ca2, _, _ := testPKI(t)
	h1, err := caHash(ca1)
	testutil.NoError(t, err)
	if !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("unexpected hash format %q", h1)
	}
	// PEM formatting does not matter, the certificate does.
	again, err := caHash(append([]byte("\n\n"), ca1...))
	testutil.NoError(t, err)
	testutil.Equal(t, again, h1)
	h2, err := caHash(ca2)
	testutil.NoError(t, err)
	if h1 == h2 {
		t.Fatal("different CAs hash equal")
	}
	_, err = caHash([]byte("not pem"))
	if err == nil {
		t.Fatal("garbage CA accepted")
	}
}
