package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

type replicationConfiguration struct {
	XMLName xml.Name          `xml:"ReplicationConfiguration"`
	Role    string            `xml:"Role,omitempty"`
	Rules   []replicationRule `xml:"Rule"`
}

type replicationRule struct {
	ID          string                 `xml:"ID,omitempty"`
	Status      string                 `xml:"Status,omitempty"`
	Priority    *int                   `xml:"Priority,omitempty"`
	Prefix      string                 `xml:"Prefix,omitempty"`
	Filter      *replicationFilter     `xml:"Filter,omitempty"`
	Destination *replicationDestination `xml:"Destination,omitempty"`
}

type replicationFilter struct {
	Prefix string             `xml:"Prefix,omitempty"`
	Tag    *replicationTag    `xml:"Tag,omitempty"`
	And    *replicationAndOp  `xml:"And,omitempty"`
}

type replicationAndOp struct {
	Prefix string           `xml:"Prefix,omitempty"`
	Tags   []replicationTag `xml:"Tag"`
}

type replicationTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type replicationDestination struct {
	Bucket       string `xml:"Bucket"`
	StorageClass string `xml:"StorageClass,omitempty"`
}

func (s *Server) putBucketReplication(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if !meta.IsVersioningActive(b.Versioning) {
		writeError(w, r, ErrInvalidRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(body) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var cfg replicationConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(cfg.Rules) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	for _, rule := range cfg.Rules {
		if rule.Destination == nil || strings.TrimSpace(rule.Destination.Bucket) == "" {
			writeError(w, r, ErrMalformedXML)
			return
		}
	}
	if err := s.Meta.SetBucketReplication(r.Context(), b.ID, body); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getBucketReplication(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	blob, err := s.Meta.GetBucketReplication(r.Context(), b.ID)
	if err != nil {
		if errors.Is(err, meta.ErrNoSuchReplication) {
			writeError(w, r, ErrNoSuchReplicationConfig)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(blob)
}

func (s *Server) deleteBucketReplication(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if err := s.Meta.DeleteBucketReplication(r.Context(), b.ID); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// replicationEventDetails carries the per-event facts the emitter needs to
// build a queue row: the source object's key, version id, tag set, and the
// event name (PUT vs CompleteMultipart) for the replication worker.
type replicationEventDetails struct {
	EventName string
	Key       string
	VersionID string
	Tags      map[string]string
}

// emitReplicationEvent walks the bucket's replication configuration; for each
// Enabled rule whose Filter matches the source key+tags, enqueues a
// replication_queue row keyed to the rule's ID and Destination.Bucket. Returns
// "PENDING" when at least one row was enqueued, "" otherwise (no config, no
// matching rule, or load error). Failures are best-effort — replication
// emission must not fail the underlying request.
func (s *Server) emitReplicationEvent(r *http.Request, b *meta.Bucket, evt replicationEventDetails) string {
	blob, err := s.Meta.GetBucketReplication(r.Context(), b.ID)
	if err != nil {
		return ""
	}
	var cfg replicationConfiguration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return ""
	}
	when := time.Now().UTC()
	matched := false
	for _, rule := range cfg.Rules {
		if rule.Status != "" && !strings.EqualFold(rule.Status, "Enabled") {
			continue
		}
		if !replicationRuleMatches(rule, evt.Key, evt.Tags) {
			continue
		}
		dest := ""
		class := ""
		if rule.Destination != nil {
			dest = rule.Destination.Bucket
			class = rule.Destination.StorageClass
		}
		row := &meta.ReplicationEvent{
			BucketID:          b.ID,
			Bucket:            b.Name,
			Key:               evt.Key,
			VersionID:         evt.VersionID,
			EventName:         evt.EventName,
			EventTime:         when,
			RuleID:            rule.ID,
			DestinationBucket: dest,
			StorageClass:      class,
		}
		_ = s.Meta.EnqueueReplication(r.Context(), row)
		matched = true
	}
	if matched {
		return "PENDING"
	}
	return ""
}

func replicationRuleMatches(rule replicationRule, key string, tags map[string]string) bool {
	prefix := rule.Prefix
	var requiredTags []replicationTag
	if rule.Filter != nil {
		switch {
		case rule.Filter.And != nil:
			prefix = rule.Filter.And.Prefix
			requiredTags = rule.Filter.And.Tags
		case rule.Filter.Tag != nil:
			requiredTags = []replicationTag{*rule.Filter.Tag}
			prefix = rule.Filter.Prefix
		default:
			prefix = rule.Filter.Prefix
		}
	}
	if prefix != "" && !strings.HasPrefix(key, prefix) {
		return false
	}
	for _, t := range requiredTags {
		if v, ok := tags[t.Key]; !ok || v != t.Value {
			return false
		}
	}
	return true
}
