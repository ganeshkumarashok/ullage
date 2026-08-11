package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Client is a read-only Kubernetes API client. It has no write methods, which
// is how the read-only promise is enforced structurally rather than by
// convention: there is no code path in this package that can mutate a cluster.
type Client struct {
	server  string
	http    *http.Client
	token   string
	context string
}

// Config selects an API server and credentials.
type Config struct {
	// APIServer overrides kubeconfig entirely. Used by the demo environment and
	// by anyone pointing ullage at a proxy.
	APIServer  string
	Token      string
	Kubeconfig string
	Context    string
	Insecure   bool
}

// New builds a client, preferring an explicit API server, then in-cluster
// credentials, then kubeconfig.
func New(cfg Config) (*Client, error) {
	if cfg.APIServer != "" {
		tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure}}
		name := cfg.Context
		if name == "" {
			name = displayName(cfg.APIServer)
		}
		return &Client{
			server:  strings.TrimRight(cfg.APIServer, "/"),
			http:    &http.Client{Transport: tr, Timeout: 60 * time.Second},
			token:   cfg.Token,
			context: name,
		}, nil
	}
	if c, err := inCluster(); err == nil {
		return c, nil
	}
	return fromKubeconfig(cfg)
}

func displayName(server string) string {
	if u, err := url.Parse(server); err == nil && u.Host != "" {
		return u.Host
	}
	return server
}

func inCluster() (*Client, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster")
	}
	const base = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(filepath.Join(base, "token"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(base, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return &Client{
		server:  fmt.Sprintf("https://%s:%s", host, port),
		http:    &http.Client{Transport: tr, Timeout: 60 * time.Second},
		token:   strings.TrimSpace(string(token)),
		context: "in-cluster",
	}, nil
}

// kubeconfig is the subset of the file format that matters here.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
			Exec                  *struct {
				Command    string   `yaml:"command"`
				Args       []string `yaml:"args"`
				APIVersion string   `yaml:"apiVersion"`
				Env        []struct {
					Name  string `yaml:"name"`
					Value string `yaml:"value"`
				} `yaml:"env"`
			} `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
}

func fromKubeconfig(cfg Config) (*Client, error) {
	path := cfg.Kubeconfig
	if path == "" {
		path = os.Getenv("KUBECONFIG")
	}
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".kube", "config")
	}
	// KUBECONFIG may be a list; the first readable entry wins, which matches
	// the common case without implementing full merge semantics.
	for _, p := range filepath.SplitList(path) {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig %s: %w", path, err)
	}

	ctxName := cfg.Context
	if ctxName == "" {
		ctxName = kc.CurrentContext
	}
	if ctxName == "" {
		return nil, fmt.Errorf("no current context in %s", path)
	}

	var clusterName, userName string
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName, userName = c.Context.Cluster, c.Context.User
		}
	}
	if clusterName == "" {
		return nil, fmt.Errorf("context %q not found in %s", ctxName, path)
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure}
	server := ""
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		server = c.Cluster.Server
		if c.Cluster.InsecureSkipTLSVerify {
			tlsCfg.InsecureSkipVerify = true
		}
		if data := c.Cluster.CertificateAuthorityData; data != "" {
			pem, err := base64.StdEncoding.DecodeString(data)
			if err == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(pem)
				tlsCfg.RootCAs = pool
			}
		} else if c.Cluster.CertificateAuthority != "" {
			if pem, err := os.ReadFile(c.Cluster.CertificateAuthority); err == nil {
				pool := x509.NewCertPool()
				pool.AppendCertsFromPEM(pem)
				tlsCfg.RootCAs = pool
			}
		}
	}
	if server == "" {
		return nil, fmt.Errorf("cluster %q has no server", clusterName)
	}

	token := ""
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		switch {
		case u.User.Token != "":
			token = u.User.Token
		case u.User.ClientCertificateData != "" && u.User.ClientKeyData != "":
			cert, _ := base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			key, _ := base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			if pair, err := tls.X509KeyPair(cert, key); err == nil {
				tlsCfg.Certificates = []tls.Certificate{pair}
			}
		case u.User.Exec != nil:
			// Managed clusters almost always authenticate through an exec
			// credential plugin (az, aws, gke-gcloud-auth-plugin). Running it is
			// the difference between working on a real cluster and not.
			cmd := exec.Command(u.User.Exec.Command, u.User.Exec.Args...)
			cmd.Env = os.Environ()
			for _, e := range u.User.Exec.Env {
				cmd.Env = append(cmd.Env, e.Name+"="+e.Value)
			}
			out, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("credential plugin %q failed: %w", u.User.Exec.Command, err)
			}
			var cred struct {
				Status struct {
					Token string `json:"token"`
				} `json:"status"`
			}
			if err := json.Unmarshal(out, &cred); err != nil {
				return nil, fmt.Errorf("credential plugin %q returned unparseable output: %w", u.User.Exec.Command, err)
			}
			token = cred.Status.Token
		}
	}

	return &Client{
		server:  strings.TrimRight(server, "/"),
		http:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}, Timeout: 60 * time.Second},
		token:   token,
		context: ctxName,
	}, nil
}

// Context is the name shown in the header. Users run against the wrong cluster;
// naming it is a safety feature.
func (c *Client) Context() string { return c.context }

// Server is the API server URL.
func (c *Client) Server() string { return c.server }

// Forbidden reports whether an error was an RBAC denial, so partial permissions
// degrade into a warning rather than a failure.
type Forbidden struct{ Path string }

func (e *Forbidden) Error() string { return "forbidden: " + e.Path }

// NotFound reports an absent API, which is how optional resources (DRA, PDBs)
// are probed without treating their absence as breakage.
type NotFound struct{ Path string }

func (e *NotFound) Error() string { return "not found: " + e.Path }

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+path, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusUnauthorized:
		return &Forbidden{Path: path}
	case http.StatusNotFound:
		return &NotFound{Path: path}
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// listAll pages through a collection until the API server stops handing back a
// continue token.
//
// The default response limit is generous but not infinite, and the clusters
// this tool is aimed at are exactly the ones large enough to hit it. A
// truncated pod list is the worst possible failure here: the missing pods are
// invisible, so the nodes they occupy look empty and get recommended for
// deletion. Paging is not an optimisation, it is a correctness requirement.
//
// pageSize is deliberately modest. Listing every pod in a 5,000-node cluster in
// one response is a large allocation on the API server, and ullage is a
// background tool that has no business causing a latency spike for the
// workloads it is measuring.
const pageSize = 500

func listAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var (
		out   []T
		token string
	)
	for page := 0; ; page++ {
		u := fmt.Sprintf("%s%slimit=%d", path, sep, pageSize)
		if token != "" {
			u += "&continue=" + url.QueryEscape(token)
		}
		var l list[T]
		if err := c.get(ctx, u, &l); err != nil {
			// A partial result is not a usable result: returning what we have
			// would understate occupancy on precisely the pages we did not see.
			return nil, err
		}
		out = append(out, l.Items...)
		token = l.Metadata.Continue
		if token == "" {
			return out, nil
		}
		if page > 1000 {
			return nil, fmt.Errorf("GET %s: still paging after %d pages, refusing to loop", path, page)
		}
	}
}

// ServerVersion returns the reported API server version. The Kubernetes minor
// version matters: DRA changes the allocation model from 1.34 onward.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	var v versionInfo
	if err := c.get(ctx, "/version", &v); err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

func (c *Client) Pods(ctx context.Context) ([]Pod, error) {
	return listAll[Pod](ctx, c, "/api/v1/pods")
}

func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	return listAll[Node](ctx, c, "/api/v1/nodes")
}

func (c *Client) Namespaces(ctx context.Context) ([]ObjectMeta, error) {
	items, err := listAll[struct {
		Metadata ObjectMeta `json:"metadata"`
	}](ctx, c, "/api/v1/namespaces")
	if err != nil {
		return nil, err
	}
	out := make([]ObjectMeta, 0, len(items))
	for _, it := range items {
		out = append(out, it.Metadata)
	}
	return out, nil
}

func (c *Client) PodDisruptionBudgets(ctx context.Context) ([]PodDisruptionBudget, error) {
	return listAll[PodDisruptionBudget](ctx, c, "/apis/policy/v1/poddisruptionbudgets")
}

// ResourceClaims lists DRA claims, trying the GA group version first and
// falling back to the beta one. An absent API means the cluster does not use
// DRA, which is a normal answer and not an error.
func (c *Client) ResourceClaims(ctx context.Context) ([]ResourceClaim, error) {
	for _, gv := range []string{"resource.k8s.io/v1", "resource.k8s.io/v1beta1", "resource.k8s.io/v1alpha3"} {
		items, err := listAll[ResourceClaim](ctx, c, "/apis/"+gv+"/resourceclaims")
		if err == nil {
			return items, nil
		}
		var nf *NotFound
		if !isNotFound(err, &nf) {
			return nil, err
		}
	}
	return nil, nil
}

func isNotFound(err error, target **NotFound) bool {
	nf, ok := err.(*NotFound)
	if ok {
		*target = nf
	}
	return ok
}

// GetObject fetches an arbitrary object by apiVersion/kind/namespace/name,
// using discovery to map the kind to its resource path. This is what lets
// provenance resolution walk into CRDs it has never seen.
func (c *Client) GetObject(ctx context.Context, apiVersion, kind, namespace, name string) (*Controller, error) {
	resource, namespaced, err := c.resourceFor(ctx, apiVersion, kind)
	if err != nil {
		return nil, err
	}
	var prefix string
	if apiVersion == "v1" {
		prefix = "/api/v1"
	} else {
		prefix = "/apis/" + apiVersion
	}
	path := prefix
	if namespaced && namespace != "" {
		path += "/namespaces/" + namespace
	}
	path += "/" + resource + "/" + name

	var obj Controller
	if err := c.get(ctx, path, &obj); err != nil {
		return nil, err
	}
	if obj.Kind == "" {
		obj.Kind = kind
	}
	if obj.APIVersion == "" {
		obj.APIVersion = apiVersion
	}
	return &obj, nil
}

var discoveryCache = map[string][]APIResource{}

func (c *Client) resourceFor(ctx context.Context, apiVersion, kind string) (string, bool, error) {
	resources, ok := discoveryCache[apiVersion]
	if !ok {
		path := "/apis/" + apiVersion
		if apiVersion == "v1" {
			path = "/api/v1"
		}
		var doc apiResourceList
		if err := c.get(ctx, path, &doc); err != nil {
			return "", false, err
		}
		resources = doc.Resources
		discoveryCache[apiVersion] = resources
	}
	for _, r := range resources {
		// Skip subresources such as deployments/status.
		if strings.Contains(r.Name, "/") {
			continue
		}
		if r.Kind == kind {
			return r.Name, r.Namespaced, nil
		}
	}
	return "", false, fmt.Errorf("kind %s not found in %s", kind, apiVersion)
}
