//go:build linux

package environment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// fieldManager names JunKloud in managedFields.
const fieldManager = "junkloud"

// apiTimeout bounds each Kubernetes API request.
const apiTimeout = 30 * time.Second

// access is what JunKloud needs to reach one cluster: its server URL, the
// CA that must sign the server certificate, and a client certificate.
type access struct {
	Server   string
	CAData   []byte
	CertData []byte
	KeyData  []byte
}

// buildAccess extracts the current context's credentials from kind's
// kubeconfig. It requires a CA and client certificate and refuses
// configurations that skip TLS verification.
func buildAccess(raw []byte) (access, error) {
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return access{}, fmt.Errorf("environment: parse kind kubeconfig: %w", err)
	}
	return accessFor(cfg, cfg.CurrentContext)
}

// loadAccess reads one named context from JunKloud's own kubeconfig file.
// It never consults $KUBECONFIG, ~/.kube/config or a current-context.
func loadAccess(path, contextName string) (access, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return access{}, fmt.Errorf("environment: read kubeconfig: %w", err)
	}
	cfg, err := clientcmd.Load(raw)
	if err != nil {
		return access{}, fmt.Errorf("environment: parse kubeconfig %s: %w", path, err)
	}
	return accessFor(cfg, contextName)
}

func accessFor(cfg *clientcmdapi.Config, contextName string) (access, error) {
	kctx, ok := cfg.Contexts[contextName]
	if !ok || contextName == "" {
		return access{}, fmt.Errorf("environment: kubeconfig has no context %q", contextName)
	}
	cluster, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return access{}, fmt.Errorf("environment: kubeconfig context %q names unknown cluster %q", contextName, kctx.Cluster)
	}
	user, ok := cfg.AuthInfos[kctx.AuthInfo]
	if !ok {
		return access{}, fmt.Errorf("environment: kubeconfig context %q names unknown user %q", contextName, kctx.AuthInfo)
	}
	if cluster.InsecureSkipTLSVerify {
		return access{}, fmt.Errorf("environment: kubeconfig context %q skips TLS verification", contextName)
	}
	if len(cluster.CertificateAuthorityData) == 0 {
		return access{}, fmt.Errorf("environment: kubeconfig context %q has no embedded CA", contextName)
	}
	if len(user.ClientCertificateData) == 0 || len(user.ClientKeyData) == 0 {
		return access{}, fmt.Errorf("environment: kubeconfig context %q has no embedded client certificate", contextName)
	}
	return access{
		Server:   cluster.Server,
		CAData:   cluster.CertificateAuthorityData,
		CertData: user.ClientCertificateData,
		KeyData:  user.ClientKeyData,
	}, nil
}

// endpointFor replaces the host of server (kind's in-network node name) with
// the node's IP, keeping the scheme and port.
func endpointFor(server, ip string) (string, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Port() == "" {
		return "", fmt.Errorf("environment: unexpected API server address %q", server)
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("environment: invalid node address %q", ip)
	}
	return "https://" + net.JoinHostPort(ip, u.Port()), nil
}

// writeKubeconfig writes JunKloud's kubeconfig (0600, atomically) with one
// cluster, user and context, all named name. current-context is set inside
// this file only, so tools pointed at it explicitly need no flag.
func writeKubeconfig(path, name, endpoint string, a access) error {
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[name] = &clientcmdapi.Cluster{Server: endpoint, CertificateAuthorityData: a.CAData}
	cfg.AuthInfos[name] = &clientcmdapi.AuthInfo{ClientCertificateData: a.CertData, ClientKeyData: a.KeyData}
	cfg.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: name}
	cfg.CurrentContext = name
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("environment: encode kubeconfig: %w", err)
	}
	return writeFileAtomic(path, raw)
}

// restConfig builds a client configuration that verifies the server
// certificate against the cluster CA.
func restConfig(a access, endpoint string) *rest.Config {
	return &rest.Config{
		Host: endpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   a.CAData,
			CertData: a.CertData,
			KeyData:  a.KeyData,
		},
		Timeout:   apiTimeout,
		UserAgent: "junkloud",
		// The doctor probes capabilities in parallel; client-go's default
		// (5 QPS, burst 10) would throttle it.
		QPS:   20,
		Burst: 40,
	}
}

// clients are the API clients bound to one verified endpoint.
type clients struct {
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	mapper meta.ResettableRESTMapper
}

func newClients(cfg *rest.Config) (*clients, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("environment: kubernetes client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("environment: dynamic client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(kube.Discovery()))
	return &clients{kube: kube, dyn: dyn, mapper: mapper}, nil
}

// caHash identifies a CA bundle: "sha256:" over the DER bytes of every
// certificate in it, so PEM formatting does not matter.
func caHash(pemData []byte) (string, error) {
	h := sha256.New()
	n := 0
	for rest := pemData; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		h.Write(block.Bytes)
		n++
	}
	if n == 0 {
		return "", fmt.Errorf("environment: no certificate in CA data")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
