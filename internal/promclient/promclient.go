// Package promclient is a tiny Prometheus query wrapper used by the admin
// console for top-N widgets and the metrics dashboard. It speaks the
// Prometheus HTTP API ("/api/v1/query", "/api/v1/query_range") with stdlib
// net/http only — no prometheus/client_golang/api dependency.
//
// The client degrades gracefully when BaseURL is empty: every method returns
// ErrUnavailable so callers can render an "—" / "Metrics unavailable" surface
// instead of crashing or 500-ing.
package promclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ErrUnavailable signals that the client has no Prometheus URL configured or
// the upstream returned an unexpected error. Callers should treat this as a
// "metrics unavailable" state, not a crash.
var ErrUnavailable = errors.New("prometheus unavailable")

// Client speaks PromQL against a Prometheus-compatible HTTP endpoint.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client. baseURL may be empty — Available() will then return
// false and every Query method returns ErrUnavailable.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Available is true when the client has a base URL configured.
func (c *Client) Available() bool {
	return c != nil && c.BaseURL != ""
}

// Sample is a single PromQL instant-vector data point: metric labels +
// timestamp + value.
type Sample struct {
	Metric    map[string]string
	Timestamp time.Time
	Value     float64
}

// Query runs an instant query (`/api/v1/query?query=...`). Returns the
// matrix result as a flat []Sample.
func (c *Client) Query(ctx context.Context, expr string) ([]Sample, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	q := url.Values{}
	q.Set("query", expr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: prometheus status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}
	var raw struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string            `json:"resultType"`
			Result     []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if raw.Status != "success" {
		return nil, fmt.Errorf("%w: prometheus status=%q", ErrUnavailable, raw.Status)
	}
	switch raw.Data.ResultType {
	case "vector":
		return parseVector(raw.Data.Result)
	case "scalar":
		return parseScalar(raw.Data.Result)
	default:
		return nil, fmt.Errorf("%w: unsupported resultType %q", ErrUnavailable, raw.Data.ResultType)
	}
}

// QueryRange runs a range query (`/api/v1/query_range`). Returns one Series
// per metric label set with [(timestamp, value)] points.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]Series, error) {
	if !c.Available() {
		return nil, ErrUnavailable
	}
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", strconv.FormatFloat(float64(start.Unix()), 'f', -1, 64))
	q.Set("end", strconv.FormatFloat(float64(end.Unix()), 'f', -1, 64))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%w: prometheus status %d: %s", ErrUnavailable, resp.StatusCode, body)
	}
	var raw struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUnavailable, err)
	}
	if raw.Status != "success" {
		return nil, fmt.Errorf("%w: prometheus status=%q", ErrUnavailable, raw.Status)
	}
	out := make([]Series, 0, len(raw.Data.Result))
	for _, r := range raw.Data.Result {
		s := Series{Metric: r.Metric, Points: make([]Point, 0, len(r.Values))}
		for _, v := range r.Values {
			ts, val, ok := decodeSamplePair(v)
			if !ok {
				continue
			}
			s.Points = append(s.Points, Point{Timestamp: ts, Value: val})
		}
		out = append(out, s)
	}
	return out, nil
}

// Series is a labelled PromQL range-query result.
type Series struct {
	Metric map[string]string
	Points []Point
}

// Point is a single (time, value) sample.
type Point struct {
	Timestamp time.Time
	Value     float64
}

func parseVector(raw []json.RawMessage) ([]Sample, error) {
	out := make([]Sample, 0, len(raw))
	for _, item := range raw {
		var v struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		}
		if err := json.Unmarshal(item, &v); err != nil {
			return nil, err
		}
		ts, val, ok := decodeSamplePair(v.Value)
		if !ok {
			continue
		}
		out = append(out, Sample{Metric: v.Metric, Timestamp: ts, Value: val})
	}
	return out, nil
}

func parseScalar(raw []json.RawMessage) ([]Sample, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v [2]any
	if err := json.Unmarshal(raw[0], &v); err != nil {
		return nil, err
	}
	ts, val, ok := decodeSamplePair(v)
	if !ok {
		return nil, nil
	}
	return []Sample{{Timestamp: ts, Value: val}}, nil
}

// decodeSamplePair turns Prometheus' JSON [<float-seconds>, "<value>"] tuple
// into typed (time, float64). The value comes through JSON as a string per
// the Prometheus convention.
func decodeSamplePair(v [2]any) (time.Time, float64, bool) {
	tsFloat, ok := v[0].(float64)
	if !ok {
		return time.Time{}, 0, false
	}
	valStr, ok := v[1].(string)
	if !ok {
		return time.Time{}, 0, false
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	sec := int64(tsFloat)
	nsec := int64((tsFloat - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC(), val, true
}
