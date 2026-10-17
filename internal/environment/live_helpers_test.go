//go:build linux

package environment

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/model"
	"github.com/JunYupK/JunKloud/internal/testutil"
)

// testCA is a certificate authority able to sign serving certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	testutil.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	testutil.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	testutil.NoError(t, err)
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// serving issues a 127.0.0.1 server certificate signed by the CA.
func (ca testCA) serving(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	testutil.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "kube-apiserver"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	testutil.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeAPI is a minimal kube-apiserver serving the objects verify reads. It
// records every request so tests can prove that nothing was written.
type fakeAPI struct {
	srv       *httptest.Server
	mu        sync.Mutex
	requests  []string
	rootCA    []byte
	markerUID string // "" = marker absent
	markerEnv string
}

func startFakeAPI(t *testing.T, servingCA testCA, rootCA []byte, markerUID, markerEnv string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{rootCA: rootCA, markerUID: markerUID, markerEnv: markerEnv}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{servingCA.serving(t)}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	cm := func(ns, name string, uid string, data map[string]string) {
		_ = json.NewEncoder(w).Encode(&corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
			Data:       data,
		})
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/kube-system/configmaps/kube-root-ca.crt":
		cm("kube-system", "kube-root-ca.crt", "root-ca-uid", map[string]string{"ca.crt": string(f.rootCA)})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/"+MarkerNamespace+"/configmaps/"+MarkerName && f.markerUID != "":
		cm(MarkerNamespace, MarkerName, f.markerUID, map[string]string{markerKeyID: f.markerEnv})
	case r.Method == http.MethodGet && r.URL.Path == "/version":
		_, _ = w.Write([]byte(`{"major":"1","minor":"37","gitVersion":"v1.37.0"}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}
}

func (f *fakeAPI) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// writes counts requests that could change the cluster.
func (f *fakeAPI) writes() int {
	n := 0
	for _, r := range f.Requests() {
		if !strings.HasPrefix(r, "GET ") {
			n++
		}
	}
	return n
}

// liveSetup describes the real world a recorded identity is checked against.
type liveSetup struct {
	psIDs        []string // what `docker ps` reports for the cluster label
	inspectLabel string   // cluster label `docker inspect` reports
	servingCA    *testCA  // CA that signs the API server certificate (nil = the recorded CA)
	rootCA       []byte   // kube-root-ca.crt content (nil = the recorded CA)
	markerUID    string
	markerEnv    string
}

const liveContainer = "0123456789abcdef"

// newLiveManager records an identity for a cluster whose API server is a
// fakeAPI on 127.0.0.1 and whose Docker view is a fake runner, both shaped by
// setup. It returns the manager, the recorded identity, the fake API and the
// fake runner.
func newLiveManager(t *testing.T, setup func(recorded model.Identity, s *liveSetup)) (*Manager, model.Identity, *fakeAPI, *fakeRunner) {
	t.Helper()
	clusterCA := newTestCA(t, "cluster-ca")
	clientCert, _, clientKey := testPKI(t)
	recordedCAHash, err := caHash(clusterCA.pem)
	testutil.NoError(t, err)
	recorded := model.Identity{
		ID: "0123456789ab", ClusterName: ClusterName("0123456789ab"), Context: ClusterName("0123456789ab"),
		CAHash: recordedCAHash, MarkerUID: "uid-ours", ContainerIDs: []string{liveContainer},
	}
	s := liveSetup{
		psIDs: []string{liveContainer}, inspectLabel: recorded.ClusterName,
		markerUID: recorded.MarkerUID, markerEnv: recorded.ID,
	}
	setup(recorded, &s)
	serving := clusterCA
	if s.servingCA != nil {
		serving = *s.servingCA
	}
	root := clusterCA.pem
	if s.rootCA != nil {
		root = s.rootCA
	}
	api := startFakeAPI(t, serving, root, s.markerUID, s.markerEnv)

	fake := &fakeRunner{respond: func(exe string, args []string) (executil.Result, error) {
		switch {
		case exe == "docker" && args[0] == "ps":
			return executil.Result{Stdout: []byte(strings.Join(s.psIDs, "\n") + "\n")}, nil
		case exe == "docker" && args[0] == "container" && args[1] == "inspect":
			out := `[{"Id":"` + args[2] + `","Name":"/` + s.inspectLabel + `-control-plane","State":{"Running":true},` +
				`"Config":{"Labels":{"io.x-k8s.kind.cluster":"` + s.inspectLabel + `","io.x-k8s.kind.role":"control-plane"}},` +
				`"NetworkSettings":{"Networks":{"kind":{"IPAddress":"127.0.0.1"}}}}]`
			return executil.Result{Stdout: []byte(out)}, nil
		}
		return executil.Result{ExitCode: 1}, executil.ErrExit
	}}
	dir := t.TempDir()
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	testutil.NoError(t, writeKubeconfig(m.KubeconfigPath(), recorded.Context, api.srv.URL,
		access{Server: api.srv.URL, CAData: clusterCA.pem, CertData: clientCert, KeyData: clientKey}))
	testutil.NoError(t, m.writeIdentity(recorded))
	return m, recorded, api, fake
}
