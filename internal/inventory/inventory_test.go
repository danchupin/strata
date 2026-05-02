package inventory_test

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/inventory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

const inventoryXML = `<?xml version="1.0" encoding="UTF-8"?>
<InventoryConfiguration>
  <Id>list1</Id>
  <IsEnabled>true</IsEnabled>
  <Destination>
    <S3BucketDestination>
      <Bucket>arn:aws:s3:::dest</Bucket>
      <Format>CSV</Format>
      <Prefix>inv</Prefix>
    </S3BucketDestination>
  </Destination>
  <Schedule>
    <Frequency>Daily</Frequency>
  </Schedule>
  <IncludedObjectVersions>Current</IncludedObjectVersions>
</InventoryConfiguration>`

const inventoryAllVersionsXML = `<?xml version="1.0" encoding="UTF-8"?>
<InventoryConfiguration>
  <Id>allv</Id>
  <IsEnabled>true</IsEnabled>
  <Destination>
    <S3BucketDestination>
      <Bucket>arn:aws:s3:::dest</Bucket>
      <Format>CSV</Format>
    </S3BucketDestination>
  </Destination>
  <Schedule>
    <Frequency>Hourly</Frequency>
  </Schedule>
  <IncludedObjectVersions>All</IncludedObjectVersions>
</InventoryConfiguration>`

func newWorker(t *testing.T) (*inventory.Worker, meta.Store, data.Backend, time.Time) {
	t.Helper()
	mem := metamem.New()
	d := datamem.New()
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	w, err := inventory.New(inventory.Config{
		Meta: mem,
		Data: d,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	return w, mem, d, now
}

func putObject(t *testing.T, m meta.Store, d data.Backend, bucketID [16]byte, key, body string) {
	t.Helper()
	manifest, err := d.PutChunks(context.Background(), strings.NewReader(body), "STANDARD")
	if err != nil {
		t.Fatalf("put chunks: %v", err)
	}
	manifest.Size = int64(len(body))
	manifest.ETag = "etag-" + key
	o := &meta.Object{
		BucketID:     bucketID,
		Key:          key,
		Size:         int64(len(body)),
		ETag:         "etag-" + key,
		ContentType:  "text/plain",
		StorageClass: "STANDARD",
		Mtime:        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		Manifest:     manifest,
		IsLatest:     true,
	}
	if err := m.PutObject(context.Background(), o, false); err != nil {
		t.Fatalf("put obj: %v", err)
	}
}

func TestRunOnceProducesManifestAndCSV(t *testing.T) {
	w, m, d, now := newWorker(t)
	ctx := context.Background()

	src, err := m.CreateBucket(ctx, "src", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create src: %v", err)
	}
	target, err := m.CreateBucket(ctx, "dest", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("create dest: %v", err)
	}
	putObject(t, m, d, src.ID, "alpha.txt", "hello world")
	putObject(t, m, d, src.ID, "beta.txt", "another body")

	if err := m.SetBucketInventoryConfig(ctx, src.ID, "list1", []byte(inventoryXML)); err != nil {
		t.Fatalf("set inventory: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run once: %v", err)
	}

	stamp := now.UTC().Format("20060102T150405Z")
	manifestKey := "inv/src/list1/" + stamp + "/manifest.json"
	dataKey := "inv/src/list1/" + stamp + "/data.csv.gz"

	mObj, err := m.GetObject(ctx, target.ID, manifestKey, "")
	if err != nil {
		t.Fatalf("manifest object: %v", err)
	}
	dObj, err := m.GetObject(ctx, target.ID, dataKey, "")
	if err != nil {
		t.Fatalf("data object: %v", err)
	}

	manifestBody := readAll(t, ctx, d, mObj)
	dataBody := readAll(t, ctx, d, dObj)

	var mj struct {
		SourceBucket      string `json:"sourceBucket"`
		DestinationBucket string `json:"destinationBucket"`
		FileFormat        string `json:"fileFormat"`
		FileSchema        string `json:"fileSchema"`
		Files             []struct {
			Key         string `json:"key"`
			Size        int64  `json:"size"`
			MD5checksum string `json:"MD5checksum"`
		} `json:"files"`
	}
	if err := json.Unmarshal(manifestBody, &mj); err != nil {
		t.Fatalf("unmarshal manifest: %v\nbody=%s", err, string(manifestBody))
	}
	if mj.SourceBucket != "src" {
		t.Errorf("source bucket: %s", mj.SourceBucket)
	}
	if mj.DestinationBucket != "arn:aws:s3:::dest" {
		t.Errorf("destination bucket: %s", mj.DestinationBucket)
	}
	if mj.FileFormat != "CSV" {
		t.Errorf("file format: %s", mj.FileFormat)
	}
	wantSchema := "Bucket, Key, VersionId, IsLatest, IsDeleteMarker, Size, LastModifiedDate, ETag, StorageClass, ChecksumAlgorithm"
	if mj.FileSchema != wantSchema {
		t.Errorf("file schema:\n got %q\nwant %q", mj.FileSchema, wantSchema)
	}
	if len(mj.Files) != 1 {
		t.Fatalf("files: want 1 got %d", len(mj.Files))
	}
	if mj.Files[0].Key != dataKey {
		t.Errorf("manifest data key mismatch: %s vs %s", mj.Files[0].Key, dataKey)
	}
	if mj.Files[0].Size != int64(len(dataBody)) {
		t.Errorf("manifest size mismatch: %d vs %d", mj.Files[0].Size, len(dataBody))
	}

	rows, err := inventory.DecodeCSVGz(dataBody)
	if err != nil {
		t.Fatalf("decode csv: %v", err)
	}
	if len(rows) < 3 {
		t.Fatalf("rows: want >=3 (header + 2 data) got %d", len(rows))
	}
	if !reflect.DeepEqual(rows[0], inventory.CSVHeader) {
		t.Errorf("header:\n got %v\nwant %v", rows[0], inventory.CSVHeader)
	}
	keys := []string{rows[1][1], rows[2][1]}
	gotAlpha, gotBeta := false, false
	for _, k := range keys {
		switch k {
		case "alpha.txt":
			gotAlpha = true
		case "beta.txt":
			gotBeta = true
		}
	}
	if !gotAlpha || !gotBeta {
		t.Errorf("missing keys; got %v", keys)
	}
	if rows[1][0] != "src" {
		t.Errorf("bucket col: %s", rows[1][0])
	}
}

func TestDueOnlyOncePerPeriod(t *testing.T) {
	w, m, d, _ := newWorker(t)
	ctx := context.Background()

	src, _ := m.CreateBucket(ctx, "src", "owner", "STANDARD")
	_, _ = m.CreateBucket(ctx, "dest", "owner", "STANDARD")
	putObject(t, m, d, src.ID, "a", "x")
	if err := m.SetBucketInventoryConfig(ctx, src.ID, "list1", []byte(inventoryXML)); err != nil {
		t.Fatalf("set inventory: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	target, _ := m.GetBucket(ctx, "dest")
	first, _ := m.ListObjects(ctx, target.ID, meta.ListOptions{Limit: 100})
	want1 := len(first.Objects)

	// Second run inside the same Daily period must NOT emit.
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	second, _ := m.ListObjects(ctx, target.ID, meta.ListOptions{Limit: 100})
	if len(second.Objects) != want1 {
		t.Errorf("second run produced extra reports: %d -> %d", want1, len(second.Objects))
	}
}

func TestDisabledConfigSkipped(t *testing.T) {
	w, m, _, _ := newWorker(t)
	ctx := context.Background()
	src, _ := m.CreateBucket(ctx, "src", "owner", "STANDARD")
	_, _ = m.CreateBucket(ctx, "dest", "owner", "STANDARD")
	disabled := strings.Replace(inventoryXML, "<IsEnabled>true</IsEnabled>", "<IsEnabled>false</IsEnabled>", 1)
	if err := m.SetBucketInventoryConfig(ctx, src.ID, "list1", []byte(disabled)); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	target, _ := m.GetBucket(ctx, "dest")
	res, _ := m.ListObjects(ctx, target.ID, meta.ListOptions{Limit: 100})
	if len(res.Objects) != 0 {
		t.Errorf("disabled config produced %d objects", len(res.Objects))
	}
}

func TestAllVersionsIncludesNonLatest(t *testing.T) {
	w, m, d, _ := newWorker(t)
	ctx := context.Background()
	src, _ := m.CreateBucket(ctx, "src", "owner", "STANDARD")
	_ = m.SetBucketVersioning(ctx, "src", meta.VersioningEnabled)
	src, _ = m.GetBucket(ctx, "src")
	_, _ = m.CreateBucket(ctx, "dest", "owner", "STANDARD")

	for _, body := range []string{"v1", "v2", "v3"} {
		manifest, err := d.PutChunks(ctx, strings.NewReader(body), "STANDARD")
		if err != nil {
			t.Fatalf("put chunks: %v", err)
		}
		manifest.Size = int64(len(body))
		o := &meta.Object{
			BucketID:     src.ID,
			Key:          "k",
			Size:         int64(len(body)),
			ETag:         "etag-" + body,
			ContentType:  "text/plain",
			StorageClass: "STANDARD",
			Mtime:        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			Manifest:     manifest,
		}
		if err := m.PutObject(ctx, o, true); err != nil {
			t.Fatalf("put obj: %v", err)
		}
	}

	if err := m.SetBucketInventoryConfig(ctx, src.ID, "allv", []byte(inventoryAllVersionsXML)); err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	target, _ := m.GetBucket(ctx, "dest")
	res, _ := m.ListObjects(ctx, target.ID, meta.ListOptions{Limit: 100})
	var dataObj *meta.Object
	for _, o := range res.Objects {
		if strings.HasSuffix(o.Key, "data.csv.gz") {
			dataObj = o
		}
	}
	if dataObj == nil {
		t.Fatalf("no csv produced; got %d objects", len(res.Objects))
	}
	body := readAll(t, ctx, d, dataObj)
	rows, err := inventory.DecodeCSVGz(body)
	if err != nil {
		t.Fatalf("decode csv: %v", err)
	}
	// 3 versions of "k" + header.
	if len(rows) != 4 {
		t.Errorf("rows: want 4 got %d (rows=%v)", len(rows), rows)
	}
}

func readAll(t *testing.T, ctx context.Context, d data.Backend, o *meta.Object) []byte {
	t.Helper()
	rc, err := d.GetChunks(ctx, o.Manifest, 0, o.Size)
	if err != nil {
		t.Fatalf("get chunks: %v", err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	return body
}
