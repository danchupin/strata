// Package replication ships the cross-cluster replication worker that drains
// meta.ReplicationEvent rows from replication_queue and copies the source
// object to a peer Strata gateway via HTTP PUT. Failed deliveries retry with
// exponential backoff; after the retry budget the source object is marked
// FAILED and replication_lag_seconds is observed.
package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// Dispatcher is the per-event copy strategy. The worker calls Send with the
// queue row plus a reader over the source object's body. Implementations must
// drain or close the reader before returning.
type Dispatcher interface {
	Send(ctx context.Context, evt meta.ReplicationEvent, src *Source) error
}

// Source carries the bytes + metadata the dispatcher writes to the peer. Body
// is the source object's data-backend stream (plaintext for unencrypted
// objects; ciphertext for SSE-encrypted ones — the peer only stores it
// verbatim, decryption is the consumer's problem).
type Source struct {
	Body         io.ReadCloser
	Size         int64
	ContentType  string
	StorageClass string
	UserMeta     map[string]string
}

// HTTPDispatcher PUTs source bytes to <scheme>://<endpoint>/<destBucket>/<key>
// over an HTTP transport. The peer is another Strata gateway accepting
// boto-compatible PUTs; auth + signing are out of scope for this story
// (operators are expected to terminate auth at the peer's edge or use a
// dedicated replication identity once US-022 lands).
type HTTPDispatcher struct {
	Client *http.Client
	Scheme string // "http" or "https"; default "https"
}

func (d *HTTPDispatcher) Send(ctx context.Context, evt meta.ReplicationEvent, src *Source) error {
	defer src.Body.Close()
	endpoint := strings.TrimSpace(evt.DestinationEndpoint)
	if endpoint == "" {
		return errors.New("replication: destination endpoint not configured on rule")
	}
	bucket := strings.TrimSpace(evt.DestinationBucket)
	if bucket == "" {
		return errors.New("replication: destination bucket missing")
	}
	scheme := d.Scheme
	if scheme == "" {
		scheme = "https"
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   endpoint,
		Path:   "/" + strings.TrimPrefix(stripBucketARN(bucket), "/") + "/" + evt.Key,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), src.Body)
	if err != nil {
		return err
	}
	req.ContentLength = src.Size
	if src.ContentType != "" {
		req.Header.Set("Content-Type", src.ContentType)
	}
	if src.StorageClass != "" {
		req.Header.Set("x-amz-storage-class", src.StorageClass)
	}
	if evt.VersionID != "" {
		req.Header.Set("x-amz-replication-source-version-id", evt.VersionID)
	}
	if evt.RuleID != "" {
		req.Header.Set("x-amz-replication-rule-id", evt.RuleID)
	}
	for k, v := range src.UserMeta {
		req.Header["x-amz-meta-"+k] = []string{v}
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("replication: peer %s returned %d: %s",
		u.String(), resp.StatusCode, strings.TrimSpace(string(preview)))
}

// stripBucketARN turns "arn:aws:s3:::dest" into "dest" so the destination
// path matches a path-style S3 PUT against the peer. AWS rule XML carries an
// ARN; the bare bucket name is what the peer router sees.
func stripBucketARN(s string) string {
	if rest, ok := strings.CutPrefix(s, "arn:aws:s3:::"); ok {
		return rest
	}
	return s
}

