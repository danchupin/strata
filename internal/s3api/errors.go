package s3api

import (
	"encoding/xml"
	"net/http"
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
	ErrExpiredToken          = APIError{Code: "ExpiredToken", Message: "The provided token has expired", Status: http.StatusForbidden}
	ErrInvalidToken          = APIError{Code: "InvalidToken", Message: "The provided token is malformed or otherwise invalid", Status: http.StatusForbidden}
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
	ErrNoSuchEncryption             = APIError{Code: "ServerSideEncryptionConfigurationNotFoundError", Message: "The server side encryption configuration was not found", Status: http.StatusNotFound}
	ErrInvalidEncryptionAlgorithm   = APIError{Code: "InvalidArgument", Message: "The encryption algorithm specified is not supported", Status: http.StatusBadRequest}
	ErrKMSNotImplemented            = APIError{Code: "NotImplemented", Message: "aws:kms server-side encryption is not supported", Status: http.StatusNotImplemented}
	ErrInvalidRequest               = APIError{Code: "InvalidRequest", Message: "The request is invalid", Status: http.StatusBadRequest}
	ErrInvalidDigest                = APIError{Code: "InvalidDigest", Message: "The provided digest does not match the supplied data", Status: http.StatusBadRequest}
	ErrSSECRequired                 = APIError{Code: "InvalidRequest", Message: "The object was stored using server-side encryption with a customer-provided key; matching SSE-C headers are required", Status: http.StatusBadRequest}
	ErrSSECKeyMismatch              = APIError{Code: "AccessDenied", Message: "The provided customer key does not match the key the object was encrypted with", Status: http.StatusBadRequest}
	ErrCORSNotEnabled               = APIError{Code: "CORSResponse", Message: "CORS is not enabled for this bucket", Status: http.StatusForbidden}
	ErrMalformedXML        = APIError{Code: "MalformedXML", Message: "The XML you provided was not well-formed", Status: http.StatusBadRequest}
	ErrMalformedACLError   = APIError{Code: "MalformedACLError", Message: "The XML you provided was not well-formed or did not validate against our published schema", Status: http.StatusBadRequest}
	ErrInvalidArgument     = APIError{Code: "InvalidArgument", Message: "Invalid argument", Status: http.StatusBadRequest}
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
		case "expired token":
			apiErr = ErrExpiredToken
		case "invalid security token":
			apiErr = ErrInvalidToken
		default:
			apiErr = ErrAccessDenied
		}
	}
	writeError(w, r, apiErr)
}
