package data

const DefaultChunkSize int64 = 4 * 1024 * 1024

type Manifest struct {
	Class     string
	Size      int64
	ChunkSize int64
	ETag      string
	Chunks    []ChunkRef
	// PartChunks records the number of chunks contributed by each part of a
	// multipart upload, in part order. Empty for single-PUT objects. Used by
	// the SSE-S3 multipart decrypt path to map a flat chunk index back to
	// (partNumber, chunkIndexInPart) so the IV input matches what was used
	// during UploadPart.
	PartChunks []int `json:",omitempty"`
	// PartChecksums records the per-part stored x-amz-checksum-<algo>
	// values in PartNumber order. Empty for single-PUT objects.
	// Populated by CompleteMultipartUpload so a `GET ?partNumber=N`
	// can echo the per-part checksum the UploadPart call originally
	// stored on multipart_parts (which is deleted after Complete).
	PartChecksums []map[string]string `json:",omitempty"`
}

type ChunkRef struct {
	Cluster   string
	Pool      string
	Namespace string `json:",omitempty"`
	OID       string
	Size      int64
}
