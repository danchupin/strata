package s3api_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// Wire-shape mirrors of the handler's response structs. Kept independent of
// the production types so a rename of an XML element name is caught here.
type doDeletedEnt struct {
	Key                   string `xml:"Key"`
	VersionID             string `xml:"VersionId"`
	DeleteMarker          bool   `xml:"DeleteMarker"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId"`
}

type doErrorEnt struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

type doResult struct {
	XMLName xml.Name       `xml:"DeleteResult"`
	Deleted []doDeletedEnt `xml:"Deleted"`
	Errors  []doErrorEnt   `xml:"Error"`
}

type delObj struct {
	key       string
	versionID string
}

// deleteBody renders a <Delete> request body. Quiet is emitted only when set so
// the default-verbose path is exercised with the AWS-canonical (absent) form.
func deleteBody(quiet bool, objs ...delObj) string {
	var b strings.Builder
	b.WriteString("<Delete>")
	if quiet {
		b.WriteString("<Quiet>true</Quiet>")
	}
	for _, o := range objs {
		b.WriteString("<Object><Key>")
		b.WriteString(o.key)
		b.WriteString("</Key>")
		if o.versionID != "" {
			b.WriteString("<VersionId>")
			b.WriteString(o.versionID)
			b.WriteString("</VersionId>")
		}
		b.WriteString("</Object>")
	}
	b.WriteString("</Delete>")
	return b.String()
}

func parseDeleteResult(t *testing.T, raw string) doResult {
	t.Helper()
	var res doResult
	if err := xml.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("unmarshal DeleteResult: %v; body=%s", err, raw)
	}
	return res
}

func deletedKeys(res doResult) []string {
	out := make([]string, 0, len(res.Deleted))
	for _, d := range res.Deleted {
		out = append(out, d.Key)
	}
	sort.Strings(out)
	return out
}

// TestDeleteObjects_Matrix pins the batch/partial-failure semantics of the
// multi-object delete handler on an unversioned bucket: quiet vs verbose,
// idempotent missing-key success rows, and the per-key <Error> wire shape.
// Closes R1 (previously only exercised incidentally by the race workload).
func TestDeleteObjects_Matrix(t *testing.T) {
	tests := []struct {
		name string
		// putKeys are PUT into a fresh unversioned bucket before the delete.
		putKeys []string
		body    string
		// expectedDeletedKeys is the sorted set of keys in <Deleted>.
		expectedDeletedKeys []string
		// expectedErrors is matched order-insensitively by (Key, Code).
		expectedErrors []doErrorEnt
		// expectedWireContains asserts raw element names when set.
		expectedWireContains []string
	}{
		{
			name:    "verbose reports existing and missing keys as deleted",
			putKeys: []string{"k1", "k2"},
			// k1 exists, k3 never existed — DeleteObjects is idempotent, both
			// land in <Deleted> with no <Error> (AWS semantics).
			body:                deleteBody(false, delObj{key: "k1"}, delObj{key: "k3"}),
			expectedDeletedKeys: []string{"k1", "k3"},
			expectedErrors:      nil,
		},
		{
			name:                "quiet suppresses deleted rows",
			putKeys:             []string{"k1"},
			body:                deleteBody(true, delObj{key: "k1"}),
			expectedDeletedKeys: []string{},
			expectedErrors:      nil,
		},
		{
			name:                "quiet still reports per-key errors",
			putKeys:             nil,
			body:                deleteBody(true, delObj{key: ""}),
			expectedDeletedKeys: []string{},
			expectedErrors:      []doErrorEnt{{Key: "", Code: "MalformedXML"}},
		},
		{
			name:    "mixed success plus per-key error wire shape",
			putKeys: []string{"k1"},
			// empty key -> per-key MalformedXML error; k1 -> deleted.
			body:                 deleteBody(false, delObj{key: "k1"}, delObj{key: ""}),
			expectedDeletedKeys:  []string{"k1"},
			expectedErrors:       []doErrorEnt{{Key: "", Code: "MalformedXML"}},
			expectedWireContains: []string{"<DeleteResult", "<Deleted>", "<Error>", "<Code>MalformedXML</Code>", "<Message>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
			for _, k := range tt.putKeys {
				h.mustStatus(h.doString("PUT", "/bkt/"+k, "body-"+k), 200)
			}

			resp := h.doString("POST", "/bkt?delete", tt.body)
			h.mustStatus(resp, http.StatusOK)
			raw := h.readBody(resp)
			res := parseDeleteResult(t, raw)

			if got := deletedKeys(res); !equalStringSlices(got, sortedCopy(tt.expectedDeletedKeys)) {
				t.Errorf("deleted keys: got %v want %v", got, tt.expectedDeletedKeys)
			}

			assertErrorRows(t, res.Errors, tt.expectedErrors)

			for _, frag := range tt.expectedWireContains {
				if !strings.Contains(raw, frag) {
					t.Errorf("wire shape missing %q in %s", frag, raw)
				}
			}

			// Idempotent-delete sanity: every key named in <Deleted> is gone.
			for _, d := range res.Deleted {
				if d.Key == "" {
					continue
				}
				h.mustStatus(h.doString("GET", "/bkt/"+d.Key, ""), 404)
			}
		})
	}
}

// TestDeleteObjects_RequestRejected covers the top-level rejection paths: the
// 1000-key cap, an empty <Delete>, and a malformed body — all 400 MalformedXML.
func TestDeleteObjects_RequestRejected(t *testing.T) {
	manyKeys := func(n int) string {
		objs := make([]delObj, n)
		for i := range objs {
			objs[i] = delObj{key: fmt.Sprintf("k%d", i)}
		}
		return deleteBody(false, objs...)
	}

	tests := []struct {
		name           string
		body           string
		expectedCode   string
		expectedStatus int
	}{
		{
			name:           "1001 keys over the cap",
			body:           manyKeys(1001),
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "MalformedXML",
		},
		{
			name:           "zero objects",
			body:           "<Delete></Delete>",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "MalformedXML",
		},
		{
			name:           "not well-formed body",
			body:           "<Delete><Object><Key>x",
			expectedStatus: http.StatusBadRequest,
			expectedCode:   "MalformedXML",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

			resp := h.doString("POST", "/bkt?delete", tt.body)
			h.mustStatus(resp, tt.expectedStatus)
			raw := h.readBody(resp)
			if !strings.Contains(raw, "<Code>"+tt.expectedCode+"</Code>") {
				t.Errorf("error code: want %s in %s", tt.expectedCode, raw)
			}
		})
	}
}

// TestDeleteObjects_CapBoundary proves the cap is strictly > 1000: exactly 1000
// objects is accepted (200) and every row is reported in verbose mode.
func TestDeleteObjects_CapBoundary(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)

	objs := make([]delObj, 1000)
	for i := range objs {
		objs[i] = delObj{key: fmt.Sprintf("k%d", i)}
	}
	resp := h.doString("POST", "/bkt?delete", deleteBody(false, objs...))
	h.mustStatus(resp, http.StatusOK)
	res := parseDeleteResult(t, h.readBody(resp))
	if len(res.Deleted) != 1000 {
		t.Fatalf("deleted rows: got %d want 1000", len(res.Deleted))
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: got %d want 0", len(res.Errors))
	}
}

// TestDeleteObjects_Versioned pins the versioned-bucket legs: an
// unversioned-style delete creates a delete marker (DeleteMarker=true +
// DeleteMarkerVersionId), an explicit ?versionId removes that specific version,
// and deleting the marker's own version restores the prior latest.
func TestDeleteObjects_Versioned(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	enableVersioning(h, "bkt")

	v1 := putObjectReturnVersion(t, h, "/bkt/doc", "v1")
	v2 := putObjectReturnVersion(t, h, "/bkt/doc", "v2")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("setup version ids: v1=%q v2=%q", v1, v2)
	}

	// Leg 1: no versionId on a versioned bucket creates a delete marker.
	resp := h.doString("POST", "/bkt?delete", deleteBody(false, delObj{key: "doc"}))
	h.mustStatus(resp, http.StatusOK)
	res := parseDeleteResult(t, h.readBody(resp))
	if len(res.Deleted) != 1 {
		t.Fatalf("marker leg: got %d deleted rows want 1: %+v", len(res.Deleted), res)
	}
	mk := res.Deleted[0]
	if mk.Key != "doc" || !mk.DeleteMarker || mk.DeleteMarkerVersionID == "" {
		t.Fatalf("marker row: got %+v want Key=doc DeleteMarker=true DeleteMarkerVersionId!=\"\"", mk)
	}
	if mk.VersionID != "" {
		t.Errorf("marker row VersionId: got %q want empty", mk.VersionID)
	}
	markerVID := mk.DeleteMarkerVersionID
	// Marker is now latest -> unversioned GET 404s.
	h.mustStatus(h.doString("GET", "/bkt/doc", ""), 404)

	// Leg 2: explicit ?versionId removes that specific version, reflected in the row.
	resp = h.doString("POST", "/bkt?delete", deleteBody(false, delObj{key: "doc", versionID: v1}))
	h.mustStatus(resp, http.StatusOK)
	res = parseDeleteResult(t, h.readBody(resp))
	if len(res.Deleted) != 1 {
		t.Fatalf("version leg: got %d deleted rows want 1: %+v", len(res.Deleted), res)
	}
	vr := res.Deleted[0]
	if vr.Key != "doc" || vr.VersionID != v1 || vr.DeleteMarker {
		t.Fatalf("version row: got %+v want Key=doc VersionId=%s DeleteMarker=false", vr, v1)
	}
	h.mustStatus(h.doString("GET", "/bkt/doc?versionId="+v1, ""), 404)

	// Leg 3: deleting the marker's own version restores v2 as latest.
	resp = h.doString("POST", "/bkt?delete", deleteBody(false, delObj{key: "doc", versionID: markerVID}))
	h.mustStatus(resp, http.StatusOK)
	res = parseDeleteResult(t, h.readBody(resp))
	if len(res.Deleted) != 1 {
		t.Fatalf("marker-removal leg: got %d deleted rows want 1: %+v", len(res.Deleted), res)
	}
	rm := res.Deleted[0]
	if !rm.DeleteMarker || rm.DeleteMarkerVersionID != markerVID {
		t.Fatalf("marker-removal row: got %+v want DeleteMarker=true DeleteMarkerVersionId=%s", rm, markerVID)
	}
	getResp := h.doString("GET", "/bkt/doc", "")
	h.mustStatus(getResp, 200)
	if body := h.readBody(getResp); body != "v2" {
		t.Errorf("restored latest: got %q want v2", body)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertErrorRows matches expected per-key error rows order-insensitively by
// (Key, Code) and asserts every error carries a non-empty Message.
func assertErrorRows(t *testing.T, got, want []doErrorEnt) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("error rows: got %d (%+v) want %d (%+v)", len(got), got, len(want), want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g.Key == w.Key && g.Code == w.Code {
				found = true
				if g.Message == "" {
					t.Errorf("error row %+v has empty Message", g)
				}
				break
			}
		}
		if !found {
			t.Errorf("missing expected error row %+v in %+v", w, got)
		}
	}
}
