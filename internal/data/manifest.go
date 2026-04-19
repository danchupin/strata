package data

const DefaultChunkSize int64 = 4 * 1024 * 1024

type Manifest struct {
	Class     string
	Size      int64
	ChunkSize int64
	ETag      string
	Chunks    []ChunkRef
}

type ChunkRef struct {
	Cluster   string
	Pool      string
	Namespace string `json:",omitempty"`
	OID       string
	Size      int64
}
