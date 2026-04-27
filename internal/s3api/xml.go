package s3api

import "encoding/xml"

type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Owner   owner    `xml:"Owner"`
	Buckets struct {
		Bucket []bucketEntry `xml:"Bucket"`
	} `xml:"Buckets"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketEntry struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type listBucketResultV1 struct {
	XMLName        xml.Name         `xml:"ListBucketResult"`
	Name           string           `xml:"Name"`
	Prefix         string           `xml:"Prefix"`
	Marker         string           `xml:"Marker"`
	NextMarker     string           `xml:"NextMarker,omitempty"`
	MaxKeys        int              `xml:"MaxKeys"`
	Delimiter      string           `xml:"Delimiter,omitempty"`
	EncodingType   string           `xml:"EncodingType,omitempty"`
	IsTruncated    bool             `xml:"IsTruncated"`
	Contents       []objectEntry    `xml:"Contents"`
	CommonPrefixes []commonPrefixEl `xml:"CommonPrefixes"`
}

type listBucketResultV2 struct {
	XMLName               xml.Name         `xml:"ListBucketResult"`
	Name                  string           `xml:"Name"`
	Prefix                string           `xml:"Prefix"`
	KeyCount              int              `xml:"KeyCount"`
	MaxKeys               int              `xml:"MaxKeys"`
	Delimiter             string           `xml:"Delimiter,omitempty"`
	EncodingType          string           `xml:"EncodingType,omitempty"`
	IsTruncated           bool             `xml:"IsTruncated"`
	ContinuationToken     string           `xml:"ContinuationToken"`
	NextContinuationToken string           `xml:"NextContinuationToken,omitempty"`
	StartAfter            string           `xml:"StartAfter,omitempty"`
	Contents              []objectEntry    `xml:"Contents"`
	CommonPrefixes        []commonPrefixEl `xml:"CommonPrefixes"`
}

type objectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type commonPrefixEl struct {
	Prefix string `xml:"Prefix"`
}

type initiateMultipartResult struct {
	XMLName           xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket            string   `xml:"Bucket"`
	Key               string   `xml:"Key"`
	UploadID          string   `xml:"UploadId"`
	ChecksumAlgorithm string   `xml:"ChecksumAlgorithm,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
}

type completeMultipartBody struct {
	XMLName xml.Name           `xml:"CompleteMultipartUpload"`
	Parts   []completeBodyPart `xml:"Part"`
}

type completeBodyPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartResult struct {
	XMLName           xml.Name `xml:"CompleteMultipartUploadResult"`
	Location          string   `xml:"Location"`
	Bucket            string   `xml:"Bucket"`
	Key               string   `xml:"Key"`
	ETag              string   `xml:"ETag"`
	ChecksumCRC32     string   `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string   `xml:"ChecksumCRC32C,omitempty"`
	ChecksumSHA1      string   `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string   `xml:"ChecksumSHA256,omitempty"`
	ChecksumCRC64NVME string   `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumType      string   `xml:"ChecksumType,omitempty"`
}

type listPartsResult struct {
	XMLName              xml.Name    `xml:"ListPartsResult"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	StorageClass         string      `xml:"StorageClass"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Parts                []partEntry `xml:"Part"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker"`
}

type partEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listUploadsResult struct {
	XMLName     xml.Name      `xml:"ListMultipartUploadsResult"`
	Bucket      string        `xml:"Bucket"`
	Prefix      string        `xml:"Prefix,omitempty"`
	MaxUploads  int           `xml:"MaxUploads"`
	IsTruncated bool          `xml:"IsTruncated"`
	Uploads     []uploadEntry `xml:"Upload"`
}

type uploadEntry struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiated    string `xml:"Initiated"`
	StorageClass string `xml:"StorageClass"`
}

type versioningConfiguration struct {
	XMLName   xml.Name `xml:"VersioningConfiguration"`
	Status    string   `xml:"Status,omitempty"`
	MfaDelete string   `xml:"MfaDelete,omitempty"`
}

type listVersionsResult struct {
	XMLName        xml.Name            `xml:"ListVersionsResult"`
	Name           string              `xml:"Name"`
	Prefix         string              `xml:"Prefix"`
	Delimiter      string              `xml:"Delimiter,omitempty"`
	KeyMarker      string              `xml:"KeyMarker"`
	MaxKeys        int                 `xml:"MaxKeys"`
	IsTruncated    bool                `xml:"IsTruncated"`
	NextKeyMarker  string              `xml:"NextKeyMarker,omitempty"`
	NextVersionID  string              `xml:"NextVersionIdMarker,omitempty"`
	Versions       []versionEntry      `xml:"Version"`
	DeleteMarkers  []deleteMarkerEntry `xml:"DeleteMarker"`
	CommonPrefixes []commonPrefixEl    `xml:"CommonPrefixes"`
}

type versionEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type deleteMarkerEntry struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  tagSet   `xml:"TagSet"`
}

type tagSet struct {
	Tags []tagEntry `xml:"Tag"`
}

type tagEntry struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type retentionConfig struct {
	XMLName         xml.Name `xml:"Retention"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

type legalHoldConfig struct {
	XMLName xml.Name `xml:"LegalHold"`
	Status  string   `xml:"Status"`
}
