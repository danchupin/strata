package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a strata-gateway over the IAM admin endpoints + the
// /admin/* HTTP surface (US-034). The CLI carries its own test-principal
// header for the in-memory gateway harness; production callers proxy through
// a SigV4-signing fronting service that injects [iam root].
type Client struct {
	Endpoint   string
	HTTPClient *http.Client
	// Principal, when set, is sent as X-Test-Principal so the in-memory
	// gateway harness binds the request to that owner. Production callers
	// leave it empty and rely on upstream SigV4.
	Principal string
	// UserAgent is sent on every request.
	UserAgent string
}

// AccessKey mirrors the IAM AccessKey XML payload returned by Create + Rotate.
type AccessKey struct {
	UserName        string    `json:"user_name" xml:"UserName"`
	AccessKeyID     string    `json:"access_key_id" xml:"AccessKeyId"`
	SecretAccessKey string    `json:"secret_access_key,omitempty" xml:"SecretAccessKey"`
	Status          string    `json:"status" xml:"Status"`
	CreateDate      string    `json:"create_date" xml:"CreateDate"`
}

// LifecycleTickResult is the payload returned by POST /admin/lifecycle/tick.
type LifecycleTickResult struct {
	OK         bool   `json:"ok"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// GCDrainResult is the payload returned by POST /admin/gc/drain.
type GCDrainResult struct {
	OK         bool  `json:"ok"`
	Drained    int   `json:"drained"`
	DurationMs int64 `json:"duration_ms"`
}

// SSERotateResult is the payload returned by POST /admin/sse/rotate.
type SSERotateResult struct {
	OK               bool   `json:"ok"`
	ActiveID         string `json:"active_id"`
	BucketsScanned   int    `json:"buckets_scanned"`
	BucketsSkipped   int    `json:"buckets_skipped"`
	ObjectsScanned   int    `json:"objects_scanned"`
	ObjectsRewrapped int    `json:"objects_rewrapped"`
	UploadsScanned   int    `json:"uploads_scanned"`
	UploadsRewrapped int    `json:"uploads_rewrapped"`
	DurationMs       int64  `json:"duration_ms"`
	Error            string `json:"error,omitempty"`
}

// ReplicateRetryResult is the payload returned by POST /admin/replicate/retry.
type ReplicateRetryResult struct {
	OK       bool   `json:"ok"`
	Bucket   string `json:"bucket"`
	Scanned  int    `json:"scanned"`
	Requeued int    `json:"requeued"`
	Error    string `json:"error,omitempty"`
}

// BucketInspectResult mirrors the JSON payload of GET /admin/bucket/inspect.
type BucketInspectResult struct {
	Name              string                     `json:"name"`
	ID                string                     `json:"id"`
	Owner             string                     `json:"owner"`
	CreatedAt         time.Time                  `json:"created_at"`
	DefaultClass      string                     `json:"default_class"`
	Versioning        string                     `json:"versioning,omitempty"`
	ACL               string                     `json:"acl,omitempty"`
	ObjectLockEnabled bool                       `json:"object_lock_enabled"`
	Region            string                     `json:"region,omitempty"`
	MfaDelete         string                     `json:"mfa_delete,omitempty"`
	Configs           map[string]json.RawMessage `json:"configs,omitempty"`
}

// HTTPError is returned when the gateway responds with a non-2xx status.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("strata-admin: http %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// CreateAccessKey calls IAM Action=CreateAccessKey for the named user.
func (c *Client) CreateAccessKey(ctx context.Context, userName string) (*AccessKey, error) {
	body := iamForm("CreateAccessKey", "UserName", userName)
	resp, err := c.iamPost(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return decodeIAMAccessKey(resp.Body, "CreateAccessKeyResponse", "CreateAccessKeyResult")
}

// RotateAccessKey calls IAM Action=RotateAccessKey for an existing key id.
func (c *Client) RotateAccessKey(ctx context.Context, accessKeyID string) (*AccessKey, error) {
	body := iamForm("RotateAccessKey", "AccessKeyId", accessKeyID)
	resp, err := c.iamPost(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	return decodeIAMAccessKey(resp.Body, "CreateAccessKeyResponse", "CreateAccessKeyResult")
}

// LifecycleTick triggers a one-shot lifecycle pass on the gateway.
func (c *Client) LifecycleTick(ctx context.Context) (*LifecycleTickResult, error) {
	var out LifecycleTickResult
	if err := c.adminJSON(ctx, http.MethodPost, "/admin/lifecycle/tick", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GCDrain triggers a one-shot GC drain pass on the gateway.
func (c *Client) GCDrain(ctx context.Context) (*GCDrainResult, error) {
	var out GCDrainResult
	if err := c.adminJSON(ctx, http.MethodPost, "/admin/gc/drain", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SSERotate triggers a master-key rotation rewrap pass on the gateway. Fails
// with an HTTP 400 when the gateway is not configured with a rotation provider.
func (c *Client) SSERotate(ctx context.Context) (*SSERotateResult, error) {
	var out SSERotateResult
	if err := c.adminJSON(ctx, http.MethodPost, "/admin/sse/rotate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReplicateRetry asks the gateway to re-emit replication events for every
// version of `bucket` whose replication-status is FAILED.
func (c *Client) ReplicateRetry(ctx context.Context, bucket string) (*ReplicateRetryResult, error) {
	q := url.Values{"bucket": {bucket}}
	var out ReplicateRetryResult
	if err := c.adminJSON(ctx, http.MethodPost, "/admin/replicate/retry?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BucketInspect returns the bucket meta + every persisted bucket-config blob.
func (c *Client) BucketInspect(ctx context.Context, bucket string) (*BucketInspectResult, error) {
	q := url.Values{"bucket": {bucket}}
	var out BucketInspectResult
	if err := c.adminJSON(ctx, http.MethodGet, "/admin/bucket/inspect?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) iamPost(ctx context.Context, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.applyHeaders(req)
	return c.httpClient().Do(req)
}

func (c *Client) adminJSON(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, body)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) applyHeaders(req *http.Request) {
	if c.Principal != "" {
		req.Header.Set("X-Test-Principal", c.Principal)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
}

func iamForm(action string, kv ...string) string {
	v := url.Values{}
	v.Set("Action", action)
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return &HTTPError{Status: resp.StatusCode, Body: string(body)}
}

// decodeIAMAccessKey parses the AWS-IAM-shaped XML envelope that wraps an
// AccessKey result. rootElem is the outer XML name ("CreateAccessKeyResponse")
// and resultElem is the inner ("CreateAccessKeyResult"). Used for both
// CreateAccessKey + RotateAccessKey since the server reuses the envelope.
func decodeIAMAccessKey(body io.Reader, rootElem, resultElem string) (*AccessKey, error) {
	var env struct {
		XMLName xml.Name
		Result  struct {
			AccessKey AccessKey `xml:"AccessKey"`
		} `xml:"CreateAccessKeyResult"`
	}
	if err := xml.NewDecoder(body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode iam response: %w", err)
	}
	_ = rootElem
	_ = resultElem
	return &env.Result.AccessKey, nil
}
