package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config configures a Client.
type Config struct {
	// Endpoint is the API server base URL (https://host:6443). When empty
	// it is derived from KUBERNETES_SERVICE_HOST/PORT.
	Endpoint string
	// CredsDir holds the serviceaccount token and ca.crt.
	CredsDir string
	// UA is the User-Agent.
	UA string

	// Now is injectable for tests.
	Now func() time.Time
	// Sleep is injectable for tests.
	Sleep func(context.Context, time.Duration) error
}

// Client is a minimal Kubernetes REST client (stdlib only).
type Client struct {
	cfg     Config
	http    *http.Client
	caBytes []byte
	ua      string
}

// New builds a Client. The CA file is read eagerly; the token is re-read
// on every request (rotation-safe, PLAN 3.2).
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, fmt.Errorf("kube: no endpoint: set --kube-apiserver or KUBERNETES_SERVICE_HOST/PORT")
		}
		cfg.Endpoint = "https://" + host + ":" + port
	}
	var ca []byte
	if cfg.CredsDir != "" {
		var err error
		ca, err = os.ReadFile(cfg.CredsDir + "/ca.crt")
		if err != nil {
			return nil, fmt.Errorf("kube: reading CA: %w", err)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if cfg.UA == "" {
		cfg.UA = "simplek8s-controller"
	}
	var tlsCfg *tls.Config
	if ca != nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, fmt.Errorf("kube: no valid CA certificates found")
		}
		tlsCfg = &tls.Config{RootCAs: pool}
	}
	c := &Client{
		cfg:     cfg,
		http:    &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
		caBytes: ca,
		ua:      cfg.UA,
	}
	return c, nil
}

// token re-reads the serviceaccount token on every call.
func (c *Client) token() (string, error) {
	b, err := os.ReadFile(c.cfg.CredsDir + "/token")
	if err != nil {
		return "", fmt.Errorf("kube: reading token: %w", err)
	}
	return string(bytes.TrimSpace(b)), nil
}

// StatusError is a non-2xx API response.
type StatusError struct {
	Code    int
	Reason  string
	Message string
	// RetryAfter is set when the response carried a Retry-After header.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kube: API error %d (%s): %s", e.Code, e.Reason, e.Message)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	return statusError(err) != nil && statusError(err).Code == http.StatusNotFound
}

// IsConflict reports a 409.
func IsConflict(err error) bool {
	return statusError(err) != nil && statusError(err).Code == http.StatusConflict
}

// IsTooManyRequests reports a 429.
func IsTooManyRequests(err error) bool {
	return statusError(err) != nil && statusError(err).Code == http.StatusTooManyRequests
}

func statusError(err error) *StatusError {
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}

// doOpts tunes a single request.
type doOpts struct {
	// Retry429 enables retrying 429 responses (server overload). The
	// Eviction endpoint must NOT retry 429: there 429 means PDB denial.
	Retry429 bool
	// MaxAttempts caps total attempts (default 6).
	MaxAttempts int
}

// Do performs one API call with retry on 429 (when allowed) and 5xx.
// body may be nil; out may be nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any, opts doOpts) error {
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 6
	}
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	var lastErr error
	backoff := 200 * time.Millisecond
	var retryAfter time.Duration
	for attempt := 0; attempt < opts.MaxAttempts; attempt++ {
		if attempt > 0 {
			wait := backoff
			if retryAfter > wait {
				wait = retryAfter // honor Retry-After (PLAN 3.2)
			}
			if err := c.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			backoff = time.Duration(math.Min(float64(backoff*2), float64(5*time.Second)))
			retryAfter = 0
		}
		err := c.attempt(ctx, method, path, query, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
		se := statusError(err)
		if se == nil {
			return err // network / context error: no retry
		}
		if se.RetryAfter > 0 {
			retryAfter = se.RetryAfter
		}
		retryable := se.Code >= 500 || (opts.Retry429 && se.Code == 429)
		if !retryable {
			return err
		}
	}
	return lastErr
}

func (c *Client) attempt(ctx context.Context, method, path string, query url.Values, payload []byte, out any) error {
	token, err := c.token()
	if err != nil {
		return err
	}
	u := c.cfg.Endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	if payload != nil && method != http.MethodGet && method != http.MethodDelete {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &StatusError{Code: resp.StatusCode}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				se.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		se.Reason = status.Reason
		se.Message = status.Message
		if se.Message == "" {
			se.Message = string(bytes.TrimSpace(data))
		}
		return se
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("kube: decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// --- Convenience verbs -------------------------------------------------

// Get fetches a single object.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, nil, out, doOpts{Retry429: true})
}

// PatchNode applies a merge patch (application/merge-patch+json) to a
// node. The patch body MUST include a fresh metadata.resourceVersion
// (PLAN 3.3.2). On 409 the caller re-reads and retries.
func (c *Client) PatchNode(ctx context.Context, name string, patch map[string]any) error {
	payload, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.patchRaw(ctx, "/api/v1/nodes/"+name, payload, nil)
}

func (c *Client) patchRaw(ctx context.Context, path string, payload []byte, out any) error {
	token, err := c.token()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.cfg.Endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &StatusError{Code: resp.StatusCode}
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		se.Reason, se.Message = status.Reason, string(bytes.TrimSpace(data))
		return se
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Evict sends a policy/v1 Eviction. Result codes (PLAN 3.2):
// 200 accepted (pod goes away up to its grace period later),
// 429 denied by PDB, 404 pod already gone. 429 is NOT retried here: it is
// a meaningful denial, not server overload.
func (c *Client) Evict(ctx context.Context, namespace, pod string) error {
	body := Eviction{
		APIVersion: "policy/v1",
		Kind:       "Eviction",
		Metadata:   ObjectMeta{Name: pod, Namespace: namespace},
	}
	return c.Do(ctx, http.MethodPost,
		"/api/v1/namespaces/"+namespace+"/pods/"+pod+"/eviction",
		nil, body, nil, doOpts{Retry429: false, MaxAttempts: 3})
}

// DeletePod deletes a pod (force-drain fallback). 404 is not an error at
// the call sites (pod already gone).
func (c *Client) DeletePod(ctx context.Context, namespace, pod string) error {
	return c.Do(ctx, http.MethodDelete, "/api/v1/namespaces/"+namespace+"/pods/"+pod, nil, nil, nil,
		doOpts{Retry429: true, MaxAttempts: 3})
}

// --- Listers (paginated, limit + continue per PLAN 3.2) ----------------

func (c *Client) listAll(ctx context.Context, path string, add func(any) (string, error)) error {
	q := url.Values{}
	q.Set("limit", "500")
	for {
		var page any
		switch path {
		case "/api/v1/nodes":
			page = &NodeList{}
		case "/api/v1/pods":
			page = &PodList{}
		case "/apis/policy/v1/poddisruptionbudgets":
			page = &PDBList{}
		}
		if err := c.Do(ctx, http.MethodGet, path, q, nil, page, doOpts{Retry429: true}); err != nil {
			return err
		}
		cont, err := add(page)
		if err != nil {
			return err
		}
		if cont == "" {
			return nil
		}
		q.Set("continue", cont)
	}
}

// ListNodes lists all nodes.
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	err := c.listAll(ctx, "/api/v1/nodes", func(o any) (string, error) {
		l := o.(*NodeList)
		nodes = append(nodes, l.Items...)
		return l.Metadata.Continue, nil
	})
	return nodes, err
}

// ListPods lists pods in all namespaces.
func (c *Client) ListPods(ctx context.Context) ([]Pod, error) {
	var pods []Pod
	err := c.listAll(ctx, "/api/v1/pods", func(o any) (string, error) {
		l := o.(*PodList)
		pods = append(pods, l.Items...)
		return l.Metadata.Continue, nil
	})
	return pods, err
}

// ListPDBs lists pod disruption budgets in all namespaces.
func (c *Client) ListPDBs(ctx context.Context) ([]PodDisruptionBudget, error) {
	var pdbs []PodDisruptionBudget
	err := c.listAll(ctx, "/apis/policy/v1/poddisruptionbudgets", func(o any) (string, error) {
		l := o.(*PDBList)
		pdbs = append(pdbs, l.Items...)
		return l.Metadata.Continue, nil
	})
	return pdbs, err
}

// --- Lease verbs (leader election, PLAN 3.1/3.2) -----------------------

const leasePath = "/apis/coordination.k8s.io/v1/namespaces/%s/leases/%s"

// GetLease fetches a lease.
func (c *Client) GetLease(ctx context.Context, ns, name string) (*Lease, error) {
	var l Lease
	if err := c.Get(ctx, fmt.Sprintf(leasePath, ns, name), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLease creates a lease (409 when it already exists). The POST goes
// to the collection path; the name comes from the body (the API server
// returns 405 for a POST to the item path).
func (c *Client) CreateLease(ctx context.Context, l *Lease) error {
	return c.Do(ctx, http.MethodPost,
		fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", l.Metadata.Namespace),
		nil, l, nil, doOpts{})
}

// UpdateLease renews/takes over a lease. The caller must set
// metadata.resourceVersion for the conditional update; on 409 the caller
// re-reads and re-evaluates (PLAN 3.2).
func (c *Client) UpdateLease(ctx context.Context, l *Lease) error {
	return c.Do(ctx, http.MethodPut, fmt.Sprintf(leasePath, l.Metadata.Namespace, l.Metadata.Name),
		nil, l, nil, doOpts{})
}

// GetConfigMap fetches a namespaced ConfigMap. 404 (IsNotFound) means
// the ConfigMap is absent: callers fall back to built-in defaults
// (PLAN-M2 3.2).
func (c *Client) GetConfigMap(ctx context.Context, ns, name string) (*ConfigMap, error) {
	var cm ConfigMap
	if err := c.Get(ctx, "/api/v1/namespaces/"+ns+"/configmaps/"+name, &cm); err != nil {
		return nil, err
	}
	return &cm, nil
}

// DeleteConfigMap deletes a namespaced ConfigMap. 404 (IsNotFound) means
// it is already gone.
func (c *Client) DeleteConfigMap(ctx context.Context, ns, name string) error {
	return c.Do(ctx, http.MethodDelete,
		"/api/v1/namespaces/"+ns+"/configmaps/"+name,
		nil, nil, nil, doOpts{Retry429: true, MaxAttempts: 3})
}

// --- Events (PLAN 3.11) --------------------------------------------------

// CreateEvent creates a v1 Event in the given namespace. Events for
// cluster-scoped subjects (Node) must be created in "default".
func (c *Client) CreateEvent(ctx context.Context, ns string, e *Event) error {
	return c.Do(ctx, http.MethodPost, "/api/v1/namespaces/"+ns+"/events",
		nil, e, nil, doOpts{})
}

// GetNode fetches a single node.
func (c *Client) GetNode(ctx context.Context, name string) (*Node, error) {
	var n Node
	if err := c.Get(ctx, "/api/v1/nodes/"+name, &n); err != nil {
		return nil, err
	}
	return &n, nil
}
