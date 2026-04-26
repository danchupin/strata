package s3api

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

type notificationConfiguration struct {
	XMLName        xml.Name            `xml:"NotificationConfiguration"`
	TopicConfigs   []notificationEntry `xml:"TopicConfiguration"`
	QueueConfigs   []notificationEntry `xml:"QueueConfiguration"`
	LambdaConfigs  []notificationEntry `xml:"CloudFunctionConfiguration"`
	LambdaConfigs2 []notificationEntry `xml:"LambdaFunctionConfiguration"`
	EventBridge    *notificationEntry  `xml:"EventBridgeConfiguration"`
}

type notificationEntry struct {
	ID             string              `xml:"Id,omitempty"`
	Topic          string              `xml:"Topic,omitempty"`
	Queue          string              `xml:"Queue,omitempty"`
	CloudFunction  string              `xml:"CloudFunction,omitempty"`
	LambdaFunction string              `xml:"LambdaFunctionArn,omitempty"`
	Events         []string            `xml:"Event"`
	Filter         *notificationFilter `xml:"Filter,omitempty"`
}

type notificationFilter struct {
	S3Key *notificationS3Key `xml:"S3Key"`
}

type notificationS3Key struct {
	Rules []notificationFilterRule `xml:"FilterRule"`
}

type notificationFilterRule struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

const (
	notifyTargetTopic       = "topic"
	notifyTargetQueue       = "queue"
	notifyTargetLambda      = "lambda"
	notifyTargetEventBridge = "eventbridge"
)

func (s *Server) putBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(body) == 0 {
		if err := s.Meta.DeleteBucketNotificationConfig(r.Context(), b.ID); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	var cfg notificationConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(cfg.TopicConfigs)+len(cfg.QueueConfigs)+len(cfg.LambdaConfigs)+len(cfg.LambdaConfigs2) == 0 && cfg.EventBridge == nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if err := s.Meta.SetBucketNotificationConfig(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketNotification(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketNotificationConfig(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchNotification) {
			writeError(w, r, ErrNoSuchNotificationConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func clientSourceIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if r.RemoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func principalFromContext(r *http.Request) string {
	info := auth.FromContext(r.Context())
	if info == nil {
		return ""
	}
	return info.Owner
}

// notificationEventDetails carries the per-event facts the emitter needs to
// build the AWS-shaped JSON Records payload.
type notificationEventDetails struct {
	EventName string
	Key       string
	Size      int64
	ETag      string
	VersionID string
	Sequencer string
	SourceIP  string
	Principal string
}

// emitNotificationEvent loads the bucket's notification config (if any),
// matches the event against each configured Topic/Queue/Lambda entry, and
// enqueues a row per matching configuration. Failures are best-effort — a
// bucket without notification config or with malformed config silently
// returns, since notification emission must not fail the underlying request.
func (s *Server) emitNotificationEvent(r *http.Request, b *meta.Bucket, evt notificationEventDetails) {
	blob, err := s.Meta.GetBucketNotificationConfig(r.Context(), b.ID)
	if err != nil {
		return
	}
	var cfg notificationConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return
	}
	when := time.Now().UTC()
	for _, e := range cfg.TopicConfigs {
		s.maybeEnqueueNotification(r, b, e, notifyTargetTopic, e.Topic, evt, when)
	}
	for _, e := range cfg.QueueConfigs {
		s.maybeEnqueueNotification(r, b, e, notifyTargetQueue, e.Queue, evt, when)
	}
	for _, e := range cfg.LambdaConfigs {
		s.maybeEnqueueNotification(r, b, e, notifyTargetLambda, e.CloudFunction, evt, when)
	}
	for _, e := range cfg.LambdaConfigs2 {
		s.maybeEnqueueNotification(r, b, e, notifyTargetLambda, e.LambdaFunction, evt, when)
	}
	if cfg.EventBridge != nil {
		s.maybeEnqueueNotification(r, b, *cfg.EventBridge, notifyTargetEventBridge, "", evt, when)
	}
}

func (s *Server) maybeEnqueueNotification(r *http.Request, b *meta.Bucket, e notificationEntry, targetType, targetARN string, evt notificationEventDetails, when time.Time) {
	if !notificationEventMatches(e.Events, evt.EventName) {
		return
	}
	if !notificationFilterMatches(e.Filter, evt.Key) {
		return
	}
	payload := buildNotificationPayload(b, evt, when, e.ID)
	row := &meta.NotificationEvent{
		BucketID:   b.ID,
		Bucket:     b.Name,
		Key:        evt.Key,
		EventName:  evt.EventName,
		EventTime:  when,
		ConfigID:   e.ID,
		TargetType: targetType,
		TargetARN:  targetARN,
		Payload:    payload,
	}
	_ = s.Meta.EnqueueNotification(r.Context(), row)
}

func notificationEventMatches(configured []string, eventName string) bool {
	if len(configured) == 0 {
		return false
	}
	for _, c := range configured {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if c == eventName {
			return true
		}
		if strings.HasSuffix(c, ":*") {
			prefix := strings.TrimSuffix(c, ":*")
			if strings.HasPrefix(eventName, prefix+":") || eventName == prefix {
				return true
			}
		}
	}
	return false
}

func notificationFilterMatches(f *notificationFilter, key string) bool {
	if f == nil || f.S3Key == nil {
		return true
	}
	for _, rule := range f.S3Key.Rules {
		switch strings.ToLower(rule.Name) {
		case "prefix":
			if !strings.HasPrefix(key, rule.Value) {
				return false
			}
		case "suffix":
			if !strings.HasSuffix(key, rule.Value) {
				return false
			}
		}
	}
	return true
}

// buildNotificationPayload renders the AWS S3 event-message JSON shape with
// a single Records entry. The full AWS schema includes glacier/userIdentity
// fields we do not populate; the worker fan-out (US-009) can extend this.
func buildNotificationPayload(b *meta.Bucket, evt notificationEventDetails, when time.Time, configID string) []byte {
	type s3Bucket struct {
		Name          string `json:"name"`
		OwnerIdentity struct {
			PrincipalID string `json:"principalId"`
		} `json:"ownerIdentity"`
		ARN string `json:"arn"`
	}
	type s3Object struct {
		Key       string `json:"key"`
		Size      int64  `json:"size,omitempty"`
		ETag      string `json:"eTag,omitempty"`
		VersionID string `json:"versionId,omitempty"`
		Sequencer string `json:"sequencer,omitempty"`
	}
	type s3Section struct {
		S3SchemaVersion string   `json:"s3SchemaVersion"`
		ConfigurationID string   `json:"configurationId,omitempty"`
		Bucket          s3Bucket `json:"bucket"`
		Object          s3Object `json:"object"`
	}
	type record struct {
		EventVersion      string         `json:"eventVersion"`
		EventSource       string         `json:"eventSource"`
		AwsRegion         string         `json:"awsRegion"`
		EventTime         string         `json:"eventTime"`
		EventName         string         `json:"eventName"`
		UserIdentity      map[string]any `json:"userIdentity,omitempty"`
		RequestParameters map[string]any `json:"requestParameters,omitempty"`
		S3                s3Section      `json:"s3"`
	}
	rec := record{
		EventVersion: "2.1",
		EventSource:  "aws:s3",
		AwsRegion:    b.Region,
		EventTime:    when.UTC().Format(time.RFC3339Nano),
		EventName:    evt.EventName,
		S3: s3Section{
			S3SchemaVersion: "1.0",
			ConfigurationID: configID,
			Bucket: s3Bucket{
				Name: b.Name,
				ARN:  "arn:aws:s3:::" + b.Name,
			},
			Object: s3Object{
				Key:       evt.Key,
				Size:      evt.Size,
				ETag:      evt.ETag,
				VersionID: evt.VersionID,
				Sequencer: evt.Sequencer,
			},
		},
	}
	rec.S3.Bucket.OwnerIdentity.PrincipalID = b.Owner
	if evt.Principal != "" {
		rec.UserIdentity = map[string]any{"principalId": evt.Principal}
	}
	if evt.SourceIP != "" {
		rec.RequestParameters = map[string]any{"sourceIPAddress": evt.SourceIP}
	}
	out := struct {
		Records []record `json:"Records"`
	}{Records: []record{rec}}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return buf
}
