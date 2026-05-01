package s3api

import (
	"encoding/xml"
	"net/http"

	"github.com/danchupin/strata/internal/auth"
)

type APIError struct {
	Code    string
	Message string
	Status  int
}

func (e APIError) Error() string { return e.Code + ": " + e.Message }

var (
	ErrSignatureDoesNotMatch = APIError{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided", Status: http.StatusForbidden}
	ErrAccessDenied          = APIError{Code: "AccessDenied", Message: "Access denied", Status: http.StatusForbidden}
	ErrInvalidAccessKeyId    = APIError{Code: "InvalidAccessKeyId", Message: "The access key Id you provided does not exist", Status: http.StatusForbidden}
	ErrRequestTimeTooSkewed  = APIError{Code: "RequestTimeTooSkewed", Message: "Request time skewed too much from server time", Status: http.StatusForbidden}
	ErrMissingAuth           = APIError{Code: "AccessDenied", Message: "Authorization is required", Status: http.StatusForbidden}
	ErrNoSuchBucket          = APIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist", Status: http.StatusNotFound}
	ErrNoSuchKey           = APIError{Code: "NoSuchKey", Message: "The specified key does not exist", Status: http.StatusNotFound}
	ErrNoSuchUpload        = APIError{Code: "NoSuchUpload", Message: "The specified multipart upload does not exist", Status: http.StatusNotFound}
	ErrBucketNotEmpty      = APIError{Code: "BucketNotEmpty", Message: "The bucket you tried to delete is not empty", Status: http.StatusConflict}
	ErrBucketExists        = APIError{Code: "BucketAlreadyOwnedByYou", Message: "Bucket already exists and is owned by you", Status: http.StatusConflict}
	ErrInvalidBucketName   = APIError{Code: "InvalidBucketName", Message: "The specified bucket name is invalid", Status: http.StatusBadRequest}
	ErrInvalidPart         = APIError{Code: "InvalidPart", Message: "One or more of the specified parts could not be found", Status: http.StatusBadRequest}
	ErrInvalidPartOrder    = APIError{Code: "InvalidPartOrder", Message: "The list of parts was not in ascending order", Status: http.StatusBadRequest}
	ErrInvalidStorageClass = APIError{Code: "InvalidStorageClass", Message: "The storage class you specified is not valid", Status: http.StatusBadRequest}
	ErrObjectLockedErr     = APIError{Code: "AccessDenied", Message: "Object is protected by object lock retention or legal hold", Status: http.StatusForbidden}
	ErrNoSuchLifecycleConfiguration = APIError{Code: "NoSuchLifecycleConfiguration", Message: "The lifecycle configuration does not exist", Status: http.StatusNotFound}
	ErrNoSuchCORSConfiguration      = APIError{Code: "NoSuchCORSConfiguration", Message: "The CORS configuration does not exist", Status: http.StatusNotFound}
	ErrNoSuchBucketPolicy           = APIError{Code: "NoSuchBucketPolicy", Message: "The bucket policy does not exist", Status: http.StatusNotFound}
	ErrNoSuchPublicAccessBlock      = APIError{Code: "NoSuchPublicAccessBlockConfiguration", Message: "The public access block configuration was not found", Status: http.StatusNotFound}
	ErrNoSuchOwnershipControls      = APIError{Code: "OwnershipControlsNotFoundError", Message: "The bucket ownership controls were not found", Status: http.StatusNotFound}
	ErrCORSNotEnabled               = APIError{Code: "CORSResponse", Message: "CORS is not enabled for this bucket", Status: http.StatusForbidden}
	ErrMalformedXML        = APIError{Code: "MalformedXML", Message: "The XML you provided was not well-formed", Status: http.StatusBadRequest}
	ErrInvalidArgument     = APIError{Code: "InvalidArgument", Message: "Invalid argument", Status: http.StatusBadRequest}
	ErrInvalidPartNumber   = APIError{Code: "InvalidPartNumber", Message: "The requested partnumber is not satisfiable", Status: http.StatusRequestedRangeNotSatisfiable}
	ErrBadDigest           = APIError{Code: "BadDigest", Message: "The checksum value supplied does not match the value Strata calculated", Status: http.StatusBadRequest}
	ErrEntityTooSmall      = APIError{Code: "EntityTooSmall", Message: "Your proposed upload is smaller than the minimum allowed object size. Each part must be at least 5 MB in size, except the last part.", Status: http.StatusBadRequest}
	ErrNotImplemented      = APIError{Code: "NotImplemented", Message: "A header you provided implies functionality that is not implemented", Status: http.StatusNotImplemented}
	ErrInternal            = APIError{Code: "InternalError", Message: "We encountered an internal error", Status: http.StatusInternalServerError}
)

type errorXML struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string
	Message   string
	Resource  string
	RequestID string
}

func writeError(w http.ResponseWriter, r *http.Request, err APIError) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(err.Status)
	_ = xml.NewEncoder(w).Encode(errorXML{
		Code:     err.Code,
		Message:  err.Message,
		Resource: r.URL.Path,
	})
}

// MapBodyError translates an error returned while reading the request
// body during a streaming-SigV4 upload into a typed APIError. Today the
// only such error is a chunk-signature mismatch (US-002): the client's
// per-chunk signature did not match the server-computed chain, so the
// gateway responds 403 SignatureDoesNotMatch and the buffered bytes from
// the mutated chunk are dropped (never reach the storage backend).
// Returns ok=false for unrecognised errors.
func MapBodyError(err error) (APIError, bool) {
	if auth.IsChunkSignatureMismatch(err) {
		return ErrSignatureDoesNotMatch, true
	}
	return APIError{}, false
}

func WriteAuthDenied(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr APIError
	switch {
	case err == nil:
		apiErr = ErrAccessDenied
	default:
		switch err.Error() {
		case "signature does not match":
			apiErr = ErrSignatureDoesNotMatch
		case "request time outside permitted window":
			apiErr = ErrRequestTimeTooSkewed
		case "credential not found":
			apiErr = ErrInvalidAccessKeyId
		case "missing Authorization header":
			apiErr = ErrMissingAuth
		default:
			apiErr = ErrAccessDenied
		}
	}
	writeError(w, r, apiErr)
}
