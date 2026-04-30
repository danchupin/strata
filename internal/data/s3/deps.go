package s3

// Blank imports pin the aws-sdk-go-v2 packages we need so go mod tidy
// keeps them in go.mod / go.sum even though US-001 only ships a stub.
// Subsequent stories replace these with real usage:
//   - feature/s3/manager  → US-002 (streaming PUT via NewUploader)
//   - service/s3          → US-002, US-003, US-004 (Put/Get/Delete RPCs)
//   - config              → US-005 (LoadDefaultConfig credential chain)
//
// When the first real import lands in any of these packages, drop the
// matching blank import here.
import (
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"
)
