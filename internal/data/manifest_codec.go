package data

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"google.golang.org/protobuf/proto"

	pb "github.com/danchupin/strata/internal/data/manifestpb"
)

// ErrEmptyManifest is returned by DecodeManifest when input is nil or zero-length.
var ErrEmptyManifest = errors.New("empty manifest blob")

// EncodeManifestJSON serialises a Manifest as JSON. This is the default
// encoder until US-049 flips the toggle to protobuf.
func EncodeManifestJSON(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// EncodeManifestProto serialises a Manifest as protobuf wire format.
func EncodeManifestProto(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return proto.Marshal(manifestToProto(m))
}

// DecodeManifest reads either a JSON-encoded or protobuf-encoded manifest.
// Format detection is based on the first non-zero byte: '{' (or '[' for an
// older array-shape that never shipped) means JSON; anything else is treated
// as protobuf wire format. proto3 never emits start_group (wire type 3) so
// the byte 0x7B ('{') cannot collide with a valid protobuf first byte.
func DecodeManifest(b []byte) (*Manifest, error) {
	if len(b) == 0 {
		return nil, nil
	}
	if isJSONManifest(b) {
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("decode manifest json: %w", err)
		}
		return &m, nil
	}
	var msg pb.Manifest
	if err := proto.Unmarshal(b, &msg); err != nil {
		return nil, fmt.Errorf("decode manifest proto: %w", err)
	}
	return manifestFromProto(&msg), nil
}

func isJSONManifest(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

func manifestToProto(m *Manifest) *pb.Manifest {
	out := &pb.Manifest{
		Class:     m.Class,
		Size:      m.Size,
		ChunkSize: m.ChunkSize,
		Etag:      m.ETag,
	}
	if len(m.Chunks) > 0 {
		out.Chunks = make([]*pb.ChunkRef, len(m.Chunks))
		for i, c := range m.Chunks {
			out.Chunks[i] = &pb.ChunkRef{
				Cluster:   c.Cluster,
				Pool:      c.Pool,
				Namespace: c.Namespace,
				Oid:       c.OID,
				Size:      c.Size,
			}
		}
	}
	if len(m.PartChunks) > 0 {
		out.PartChunks = make([]int64, len(m.PartChunks))
		for i, n := range m.PartChunks {
			out.PartChunks[i] = int64(n)
		}
	}
	if len(m.PartChecksums) > 0 {
		out.PartChecksums = make([]*pb.PartChecksum, len(m.PartChecksums))
		for i, mp := range m.PartChecksums {
			pc := &pb.PartChecksum{}
			if len(mp) > 0 {
				pc.Values = make(map[string]string, len(mp))
				maps.Copy(pc.Values, mp)
			}
			out.PartChecksums[i] = pc
		}
	}
	return out
}

func manifestFromProto(p *pb.Manifest) *Manifest {
	out := &Manifest{
		Class:     p.GetClass(),
		Size:      p.GetSize(),
		ChunkSize: p.GetChunkSize(),
		ETag:      p.GetEtag(),
	}
	if pc := p.GetChunks(); len(pc) > 0 {
		out.Chunks = make([]ChunkRef, len(pc))
		for i, c := range pc {
			out.Chunks[i] = ChunkRef{
				Cluster:   c.GetCluster(),
				Pool:      c.GetPool(),
				Namespace: c.GetNamespace(),
				OID:       c.GetOid(),
				Size:      c.GetSize(),
			}
		}
	}
	if pp := p.GetPartChunks(); len(pp) > 0 {
		out.PartChunks = make([]int, len(pp))
		for i, n := range pp {
			out.PartChunks[i] = int(n)
		}
	}
	if ps := p.GetPartChecksums(); len(ps) > 0 {
		out.PartChecksums = make([]map[string]string, len(ps))
		for i, pc := range ps {
			vals := pc.GetValues()
			if len(vals) == 0 {
				continue
			}
			cp := make(map[string]string, len(vals))
			maps.Copy(cp, vals)
			out.PartChecksums[i] = cp
		}
	}
	return out
}
