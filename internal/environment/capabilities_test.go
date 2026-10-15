//go:build linux

package environment

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/JunYupK/JunKloud/internal/executil"
	"github.com/JunYupK/JunKloud/internal/testutil"
	"github.com/JunYupK/JunKloud/runtime/platform"
)

type lockImage struct {
	Version        string            `json:"version"`
	Image          string            `json:"image"`
	Digest         string            `json:"digest"`
	RunsOn         string            `json:"runs_on"`
	Platforms      map[string]string `json:"platforms"`
	Manifest       string            `json:"manifest"`
	ManifestSHA256 string            `json:"manifest_sha256"`
}

func readPlatformLock(t *testing.T) map[string]lockImage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "runtime", "versions.lock.yaml"))
	testutil.NoError(t, err)
	var lock struct {
		Platform map[string]lockImage `json:"platform"`
	}
	testutil.NoError(t, yaml.Unmarshal(raw, &lock))
	return lock.Platform
}

// Every pinned platform image equals the lock entry, is pinned by digest and
// is published for both target architectures.
func TestPlatformImagesMatchLock(t *testing.T) {
	lock := readPlatformLock(t)
	for key, want := range map[string]struct {
		img    PinnedImage
		runsOn string
	}{
		"cloud_provider_kind": {CloudProviderKindImage, "host"},
		"envoy_proxy":         {EnvoyProxyImage, "host"},
		"metrics_server":      {MetricsServerImage, "node"},
		"probe_image":         {ProbeImage, "node"},
	} {
		img := want.img
		l, ok := lock[key]
		if !ok {
			t.Fatalf("lock has no platform.%s", key)
		}
		testutil.Equal(t, img.Name, l.Image)
		testutil.Equal(t, img.Digest, l.Digest)
		testutil.NoError(t, img.validate())
		var amd, arm bool
		for p := range l.Platforms {
			amd = amd || p == "linux/amd64"
			arm = arm || strings.HasPrefix(p, "linux/arm64")
		}
		if !amd || !arm {
			t.Fatalf("platform.%s does not record both linux/amd64 and linux/arm64: %v", key, l.Platforms)
		}
		testutil.Equal(t, l.RunsOn, want.runsOn)
	}
	testutil.Equal(t, lock["cloud_provider_kind"].Version, CloudProviderKindVersion)
	testutil.Equal(t, lock["metrics_server"].Version, MetricsServerVersion)
}

// The vendored metrics-server manifest is byte-for-byte the pinned release
// asset. The objects JunKloud applies from it reference the pinned image,
// carry ownership labels and verify TLS on both hops: metrics-server checks
// kubelet certificates against the cluster CA (no --kubelet-insecure-tls),
// and the API server checks metrics-server's certificate against a
// JunKloud CA (caBundle instead of the upstream insecureSkipTLSVerify).
func TestMetricsServerManifestPinned(t *testing.T) {
	lock := readPlatformLock(t)
	raw, err := fs.ReadFile(platform.FS, platform.MetricsServerManifest)
	testutil.NoError(t, err)
	sum := sha256.Sum256(raw)
	testutil.Equal(t, hex.EncodeToString(sum[:]), lock["metrics_server"].ManifestSHA256)
	testutil.Equal(t, "runtime/platform/"+platform.MetricsServerManifest, lock["metrics_server"].Manifest)

	tlsBundle, err := newMetricsTLS(time.Now())
	testutil.NoError(t, err)
	objs, err := platformObjects("0123456789ab", tlsBundle)
	testutil.NoError(t, err)
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.GetKind())
		testutil.Equal(t, o.GetLabels()[LabelEnvironmentID], "0123456789ab")
		testutil.Equal(t, o.GetLabels()[LabelComponent], ComponentPlatform)
		testutil.Equal(t, o.GetLabels()[LabelManagedBy], ManagedByValue)
		js := mustJSON(t, o.Object)
		if strings.Contains(strings.ToLower(js), "insecure") {
			t.Fatalf("%s %s disables TLS verification: %s", o.GetKind(), o.GetName(), js)
		}
		switch o.GetKind() {
		case "Deployment":
			for _, want := range []string{`"image":"` + MetricsServerImage.Name + `"`, "--tls-cert-file=", "--tls-private-key-file="} {
				if !strings.Contains(js, want) {
					t.Fatalf("metrics-server deployment lacks %s: %s", want, js)
				}
			}
		case "APIService":
			ca, _, _ := unstructured.NestedString(o.Object, "spec", "caBundle")
			testutil.Equal(t, ca, base64.StdEncoding.EncodeToString(tlsBundle.caPEM))
		}
	}
	for _, want := range []string{"ServiceAccount", "ClusterRole", "Deployment", "Service", "APIService", "Secret"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("metrics-server objects %v lack %s", kinds, want)
		}
	}
	// The serving certificate names the Service the API server dials and
	// chains to the CA in the APIService.
	block, _ := pem.Decode(tlsBundle.certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	testutil.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tlsBundle.caPEM)
	_, err = cert.Verify(x509.VerifyOptions{DNSName: "metrics-server.kube-system.svc", Roots: pool})
	testutil.NoError(t, err)

	// The image-build probe context is embedded and builds FROM the probe image.
	df, err := fs.ReadFile(platform.FS, platform.ImageBuildProbeDir+"/Dockerfile")
	testutil.NoError(t, err)
	if !strings.Contains(string(df), "FROM "+ProbeImage.Ref()) {
		t.Fatalf("image-build probe does not build FROM the pinned probe image:\n%s", df)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	testutil.NoError(t, err)
	j, err := yaml.YAMLToJSON(b)
	testutil.NoError(t, err)
	return string(j)
}

func TestPinnedImageValidation(t *testing.T) {
	good := PinnedImage{Name: "docker.io/library/busybox:1.37.0", Digest: "sha256:" + strings.Repeat("a", 64)}
	testutil.NoError(t, good.validate())
	testutil.Equal(t, good.Ref(), "docker.io/library/busybox:1.37.0@sha256:"+strings.Repeat("a", 64))
	for _, bad := range []PinnedImage{
		{Name: "--help:1", Digest: good.Digest},
		{Name: "busybox", Digest: good.Digest},        // no tag
		{Name: "busybox:latest", Digest: good.Digest}, // floating tag
		{Name: good.Name, Digest: "sha256:abc"},
		{Name: good.Name, Digest: ""},
	} {
		if err := bad.validate(); !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("%+v accepted (err %v)", bad, err)
		}
	}
}

// Preloading follows R6: pull the exact digest for the node's platform, tag
// it, save that platform only and load the archive into this cluster.
func TestPreloadCommands(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeRunner{respond: func(string, []string) (executil.Result, error) { return executil.Result{}, nil }}
	m := New(dir, openTestStore(t, dir), WithRunner(fake))
	img := PinnedImage{Name: "docker.io/library/busybox:1.37.0", Digest: "sha256:" + strings.Repeat("b", 64)}
	testutil.NoError(t, m.preload(bg, "junkloud-0123456789ab", "arm64", img, true))
	calls := fake.Calls()
	testutil.Equal(t, len(calls), 4)
	testutil.Equal(t, calls[0], "docker pull --quiet --platform linux/arm64 "+img.Ref())
	testutil.Equal(t, calls[1], "docker tag "+img.Ref()+" "+img.Name)
	if !strings.HasPrefix(calls[2], "docker save --platform linux/arm64 --output ") || !strings.HasSuffix(calls[2], " "+img.Name) {
		t.Fatalf("save: %s", calls[2])
	}
	archive := strings.Fields(calls[2])[5]
	testutil.Equal(t, calls[3], "kind load image-archive "+archive+" --name junkloud-0123456789ab")
	if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive %s left behind (%v)", archive, err)
	}

	// A host image is pulled and tagged but never loaded into the node.
	fake2 := &fakeRunner{respond: fake.respond}
	m2 := New(dir, openTestStore(t, t.TempDir()), WithRunner(fake2))
	testutil.NoError(t, m2.preload(bg, "junkloud-0123456789ab", "amd64", img, false))
	testutil.Equal(t, fake2.Calls(), []string{
		"docker pull --quiet --platform linux/amd64 " + img.Ref(),
		"docker tag " + img.Ref() + " " + img.Name,
	})

	// An invalid reference never reaches docker.
	fake3 := &fakeRunner{}
	m3 := New(dir, openTestStore(t, t.TempDir()), WithRunner(fake3))
	testutil.ErrorIs(t, m3.preload(bg, "junkloud-0123456789ab", "amd64", PinnedImage{Name: "-x:1", Digest: img.Digest}, true), ErrInvalidImage)
	testutil.Equal(t, len(fake3.Calls()), 0)
}

// The kind configuration always asks kubelets for CA-signed serving
// certificates (so metrics-server can verify them) and keeps host-compat
// patches opt-in.
func TestKindConfigServerTLSBootstrap(t *testing.T) {
	for _, compat := range []HostCompat{HostCompatNone, HostCompatCgroupV1} {
		cfg := string(kindConfig("0123456789ab", compat))
		var parsed map[string]any
		testutil.NoError(t, yaml.Unmarshal([]byte(cfg), &parsed))
		node := parsed["nodes"].([]any)[0].(map[string]any)
		patches := node["kubeadmConfigPatches"].([]any)
		testutil.Equal(t, len(patches), 1)
		var kubelet map[string]any
		testutil.NoError(t, yaml.Unmarshal([]byte(patches[0].(string)), &kubelet))
		testutil.Equal(t, kubelet["kind"], "KubeletConfiguration")
		testutil.Equal(t, kubelet["serverTLSBootstrap"], true)
		_, hasCgroup := kubelet["failCgroupV1"]
		testutil.Equal(t, hasCgroup, compat.Has(HostCompatCgroupV1))
	}
}

func TestParseHostCompatSet(t *testing.T) {
	got, err := ParseHostCompat("no-ipv6,cgroupv1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, HostCompat("cgroupv1,no-ipv6"))
	testutil.Equal(t, got.Has(HostCompatCgroupV1), true)
	testutil.Equal(t, got.Has(HostCompatNoIPv6), true)
	testutil.Equal(t, HostCompatCgroupV1.Has(HostCompatNoIPv6), false)
	testutil.Equal(t, HostCompatNone.Has(HostCompatCgroupV1), false)
	got, err = ParseHostCompat("cgroupv1,cgroupv1")
	testutil.NoError(t, err)
	testutil.Equal(t, got, HostCompatCgroupV1)
	if _, err := ParseHostCompat("cgroupv1,bogus"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

// cloud-provider-kind runs from the pinned image, on the kind network, with
// JunKloud ownership labels; the IPv6 sysctl shim exists only under the
// explicit no-ipv6 host-compat mode.
func TestCloudProviderRunArgs(t *testing.T) {
	args := cloudProviderRunArgs("0123456789ab", HostCompatNone)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --detach --name junkloud-cpk-0123456789ab",
		"--label " + LabelEnvironmentID + "=0123456789ab",
		"--label " + LabelManagedBy + "=" + ManagedByValue,
		"--label " + LabelComponent + "=" + ComponentCloudProvider,
		"--network kind",
		"--restart unless-stopped",
		"--volume /var/run/docker.sock:/var/run/docker.sock",
		CloudProviderKindImage.Ref() + " --gateway-channel=standard --enable-default-ingress=true",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("cloud-provider-kind args lack %q:\n%s", want, joined)
		}
	}
	for _, bad := range []string{"--privileged", "ipv6", "--entrypoint"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("default cloud-provider-kind args contain %q:\n%s", bad, joined)
		}
	}
	shim := strings.Join(cloudProviderRunArgs("0123456789ab", HostCompat("cgroupv1,no-ipv6")), " ")
	for _, want := range []string{"--entrypoint /bin/sh", "--sysctl=net.ipv6.*", "exec /bin/cloud-provider-kind"} {
		if !strings.Contains(shim, want) {
			t.Fatalf("no-ipv6 args lack %q:\n%s", want, shim)
		}
	}
	if configLabel(args) == configLabel(cloudProviderRunArgs("0123456789ab", HostCompatNoIPv6)) {
		t.Fatal("config label does not change with the run configuration")
	}
}

func configLabel(args []string) string {
	for i, a := range args {
		if a == "--label" && strings.HasPrefix(args[i+1], labelConfig+"=") {
			return args[i+1]
		}
	}
	return ""
}

// Only kubelet-serving CSRs requested by one of this environment's nodes for
// exactly that node's names and addresses are approved.
func TestKubeletServingCSRValidation(t *testing.T) {
	nodes := map[string]nodeNames{"junkloud-x-control-plane": {dns: []string{"junkloud-x-control-plane"}, ips: []string{"172.18.0.2"}}}
	good := func() *certv1.CertificateSigningRequest {
		return testCSR(t, "system:node:junkloud-x-control-plane", "system:node:junkloud-x-control-plane", []string{"system:nodes"},
			[]string{"junkloud-x-control-plane"}, []string{"172.18.0.2"},
			[]certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageServerAuth})
	}
	testutil.NoError(t, validKubeletServingCSR(good(), nodes))

	cases := map[string]func(c *certv1.CertificateSigningRequest){
		"other signer": func(c *certv1.CertificateSigningRequest) {
			c.Spec.SignerName = certv1.KubeAPIServerClientKubeletSignerName
		},
		"foreign node":  func(c *certv1.CertificateSigningRequest) { c.Spec.Username = "system:node:someone-else" },
		"not a node":    func(c *certv1.CertificateSigningRequest) { c.Spec.Username = "kubernetes-admin" },
		"no node group": func(c *certv1.CertificateSigningRequest) { c.Spec.Groups = []string{"system:authenticated"} },
		"client usage": func(c *certv1.CertificateSigningRequest) {
			c.Spec.Usages = append(c.Spec.Usages, certv1.UsageClientAuth)
		},
		"already approved": func(c *certv1.CertificateSigningRequest) {
			c.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateApproved, Status: "True"}}
		},
	}
	for name, mutate := range cases {
		c := good()
		mutate(c)
		if err := validKubeletServingCSR(c, nodes); err == nil {
			t.Fatalf("%s: CSR accepted", name)
		}
	}
	for name, c := range map[string]*certv1.CertificateSigningRequest{
		"foreign SAN IP": testCSR(t, "system:node:junkloud-x-control-plane", "system:node:junkloud-x-control-plane", []string{"system:nodes"},
			[]string{"junkloud-x-control-plane"}, []string{"10.0.0.9"}, []certv1.KeyUsage{certv1.UsageServerAuth}),
		"foreign SAN DNS": testCSR(t, "system:node:junkloud-x-control-plane", "system:node:junkloud-x-control-plane", []string{"system:nodes"},
			[]string{"evil.example"}, []string{"172.18.0.2"}, []certv1.KeyUsage{certv1.UsageServerAuth}),
		"CN mismatch": testCSR(t, "system:node:junkloud-x-control-plane", "system:node:other", []string{"system:nodes"},
			[]string{"junkloud-x-control-plane"}, []string{"172.18.0.2"}, []certv1.KeyUsage{certv1.UsageServerAuth}),
	} {
		if err := validKubeletServingCSR(c, nodes); err == nil {
			t.Fatalf("%s: CSR accepted", name)
		}
	}
}

func testCSR(t *testing.T, username, cn string, groups, dns, ips []string, usages []certv1.KeyUsage) *certv1.CertificateSigningRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	testutil.NoError(t, err)
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn, Organization: []string{"system:nodes"}}, DNSNames: dns}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	testutil.NoError(t, err)
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "csr-test"},
		Spec: certv1.CertificateSigningRequestSpec{
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
			SignerName: certv1.KubeletServingSignerName,
			Username:   username,
			Groups:     append(groups, "system:authenticated"),
			Usages:     usages,
		},
	}
}

func TestDoctorReportUnavailable(t *testing.T) {
	r := DoctorReport{Checks: []CapabilityCheck{
		{Name: CapabilityDNS, Status: StatusPass},
		{Name: CapabilityIngress, Status: StatusFail, Reason: "x"},
	}}
	testutil.Equal(t, r.Unavailable([]string{CapabilityDNS}), []string(nil))
	testutil.Equal(t, r.Unavailable([]string{CapabilityIngress, CapabilityDNS, CapabilityMetrics}), []string{CapabilityIngress, CapabilityMetrics})
	testutil.Equal(t, Capabilities, []string{"dns", "service", "network-policy", "ingress", "storage", "metrics", "image-build"})
}
