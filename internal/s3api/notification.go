package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/danchupin/strata/internal/meta"
)

type notificationConfiguration struct {
	XMLName        xml.Name             `xml:"NotificationConfiguration"`
	TopicConfigs   []notificationStub   `xml:"TopicConfiguration"`
	QueueConfigs   []notificationStub   `xml:"QueueConfiguration"`
	LambdaConfigs  []notificationStub   `xml:"CloudFunctionConfiguration"`
	LambdaConfigs2 []notificationStub   `xml:"LambdaFunctionConfiguration"`
	EventBridge    *notificationStub    `xml:"EventBridgeConfiguration"`
}

type notificationStub struct {
	Inner []byte `xml:",innerxml"`
}

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
	// Empty body clears the configuration.
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
