package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

// Recognised values of the x-amz-object-attributes request header.
const (
	objectAttrETag         = "ETag"
	objectAttrChecksum     = "Checksum"
	objectAttrObjectParts  = "ObjectParts"
	objectAttrStorageClass = "StorageClass"
	objectAttrObjectSize   = "ObjectSize"
)

var validObjectAttributes = map[string]struct{}{
	objectAttrETag:         {},
	objectAttrChecksum:     {},
	objectAttrObjectParts:  {},
	objectAttrStorageClass: {},
	objectAttrObjectSize:   {},
}

type getObjectAttributesResult struct {
	XMLName      xml.Name              `xml:"GetObjectAttributesOutput"`
	ETag         string                `xml:"ETag,omitempty"`
	Checksum     *objectAttrChecksums  `xml:"Checksum,omitempty"`
	ObjectParts  *objectAttrParts      `xml:"ObjectParts,omitempty"`
	StorageClass string                `xml:"StorageClass,omitempty"`
	ObjectSize   *int64                `xml:"ObjectSize,omitempty"`
}

type objectAttrChecksums struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
}

type objectAttrParts struct {
	PartsCount           int  `xml:"PartsCount"`
	MaxParts             int  `xml:"MaxParts,omitempty"`
	PartNumberMarker     int  `xml:"PartNumberMarker,omitempty"`
	NextPartNumberMarker int  `xml:"NextPartNumberMarker,omitempty"`
	IsTruncated          bool `xml:"IsTruncated,omitempty"`
}

func (s *Server) getObjectAttributes(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	hdr := r.Header.Get("x-amz-object-attributes")
	if hdr == "" {
		writeError(w, r, ErrInvalidRequest)
		return
	}
	requested := make(map[string]struct{})
	for _, raw := range strings.Split(hdr, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := validObjectAttributes[name]; !ok {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		requested[name] = struct{}{}
	}
	if len(requested) == 0 {
		writeError(w, r, ErrInvalidRequest)
		return
	}

	versionID := r.URL.Query().Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if o.SSECKeyMD5 != "" {
		if apiErr, ok := requireSSECMatch(r, o.SSECKeyMD5); !ok {
			writeError(w, r, apiErr)
			return
		}
	}

	resp := getObjectAttributesResult{}
	if _, ok := requested[objectAttrETag]; ok {
		resp.ETag = o.ETag
	}
	if _, ok := requested[objectAttrStorageClass]; ok {
		resp.StorageClass = o.StorageClass
	}
	if _, ok := requested[objectAttrObjectSize]; ok {
		size := o.Size
		resp.ObjectSize = &size
	}
	if _, ok := requested[objectAttrChecksum]; ok {
		if c := buildChecksumAttrs(o.Checksums); c != nil {
			resp.Checksum = c
		}
	}
	if _, ok := requested[objectAttrObjectParts]; ok {
		if op := buildObjectPartsAttrs(o); op != nil {
			resp.ObjectParts = op
		}
	}

	w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
	if o.VersionID != "" && meta.IsVersioningActive(b.Versioning) {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}
	writeXML(w, http.StatusOK, resp)
}

func buildChecksumAttrs(sums map[string]string) *objectAttrChecksums {
	if len(sums) == 0 {
		return nil
	}
	out := &objectAttrChecksums{
		ChecksumCRC32:     sums["CRC32"],
		ChecksumCRC32C:    sums["CRC32C"],
		ChecksumSHA1:      sums["SHA1"],
		ChecksumSHA256:    sums["SHA256"],
		ChecksumCRC64NVME: sums["CRC64NVME"],
	}
	if out.ChecksumCRC32 == "" && out.ChecksumCRC32C == "" && out.ChecksumSHA1 == "" && out.ChecksumSHA256 == "" && out.ChecksumCRC64NVME == "" {
		return nil
	}
	return out
}

// buildObjectPartsAttrs derives the multipart part count from the ETag suffix
// (form "<hex>-<N>"). After CompleteMultipartUpload the per-part rows are
// deleted so the parts list itself is empty; PartsCount alone is sufficient
// for clients that only need the multipart fingerprint.
func buildObjectPartsAttrs(o *meta.Object) *objectAttrParts {
	n := multipartPartsFromETag(o.ETag)
	if n <= 0 {
		return nil
	}
	return &objectAttrParts{PartsCount: n}
}

func multipartPartsFromETag(etag string) int {
	i := strings.LastIndex(etag, "-")
	if i < 0 || i == len(etag)-1 {
		return 0
	}
	n, err := strconv.Atoi(etag[i+1:])
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
