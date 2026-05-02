package s3api

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Access Points are account-scoped, named bindings to a single bucket carrying
// their own policy + PublicAccessBlock. Surfaced via ?Action= form-encoded
// requests gated on [iam root]. Schema/CRUD lives on meta.Store; this file is
// the wire layer.

const accessPointXMLNS = "http://awss3control.amazonaws.com/doc/2018-08-20/"

const (
	accessPointOriginInternet = "Internet"
	accessPointOriginVPC      = "VPC"
)

type accessPointEntry struct {
	Name              string `xml:"Name"`
	NetworkOrigin     string `xml:"NetworkOrigin"`
	VpcConfiguration  *struct {
		VpcID string `xml:"VpcId"`
	} `xml:"VpcConfiguration,omitempty"`
	Bucket         string `xml:"Bucket"`
	AccessPointArn string `xml:"AccessPointArn"`
	Alias          string `xml:"Alias"`
}

type createAccessPointResponse struct {
	XMLName  xml.Name                `xml:"CreateAccessPointResponse"`
	XMLNS    string                  `xml:"xmlns,attr"`
	Result   createAccessPointResult `xml:"CreateAccessPointResult"`
	Metadata iamResponseMetadata     `xml:"ResponseMetadata"`
}

type createAccessPointResult struct {
	AccessPointArn string `xml:"AccessPointArn"`
	Alias          string `xml:"Alias"`
}

type getAccessPointResponse struct {
	XMLName  xml.Name             `xml:"GetAccessPointResponse"`
	XMLNS    string               `xml:"xmlns,attr"`
	Result   getAccessPointResult `xml:"GetAccessPointResult"`
	Metadata iamResponseMetadata  `xml:"ResponseMetadata"`
}

type getAccessPointResult struct {
	Name              string `xml:"Name"`
	Bucket            string `xml:"Bucket"`
	NetworkOrigin     string `xml:"NetworkOrigin"`
	VpcConfiguration  *struct {
		VpcID string `xml:"VpcId"`
	} `xml:"VpcConfiguration,omitempty"`
	CreationDate string `xml:"CreationDate"`
	Alias        string `xml:"Alias"`
	AccessPointArn string `xml:"AccessPointArn"`
}

type deleteAccessPointResponse struct {
	XMLName  xml.Name            `xml:"DeleteAccessPointResponse"`
	XMLNS    string              `xml:"xmlns,attr"`
	Metadata iamResponseMetadata `xml:"ResponseMetadata"`
}

type listAccessPointsResponse struct {
	XMLName  xml.Name               `xml:"ListAccessPointsResponse"`
	XMLNS    string                 `xml:"xmlns,attr"`
	Result   listAccessPointsResult `xml:"ListAccessPointsResult"`
	Metadata iamResponseMetadata    `xml:"ResponseMetadata"`
}

type listAccessPointsResult struct {
	AccessPointList struct {
		AccessPoint []accessPointEntry `xml:"AccessPoint"`
	} `xml:"AccessPointList"`
}

func accessPointArn(name string) string {
	return "arn:aws:s3:::accesspoint/" + name
}

// newAccessPointAlias mints a fresh `ap-<random12>` alias. Random source is
// crypto/rand to avoid alias collisions across nodes.
func newAccessPointAlias() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:]))
	if len(enc) > 12 {
		enc = enc[:12]
	}
	return "ap-" + enc
}

func accessPointEntryFromMeta(ap *meta.AccessPoint) accessPointEntry {
	out := accessPointEntry{
		Name:           ap.Name,
		NetworkOrigin:  ap.NetworkOrigin,
		Bucket:         ap.Bucket,
		AccessPointArn: accessPointArn(ap.Name),
		Alias:          ap.Alias,
	}
	if ap.NetworkOrigin == accessPointOriginVPC && ap.VPCID != "" {
		out.VpcConfiguration = &struct {
			VpcID string `xml:"VpcId"`
		}{VpcID: ap.VPCID}
	}
	return out
}

func (s *Server) accessPointCreate(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "Name")
	bucket := iamParam(r, "Bucket")
	if name == "" || bucket == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	origin := accessPointOriginInternet
	vpcID := iamParam(r, "VpcConfiguration.VpcId")
	if vpcID == "" {
		vpcID = iamParam(r, "VpcId")
	}
	if vpcID != "" {
		origin = accessPointOriginVPC
	}
	ap := &meta.AccessPoint{
		Name:              name,
		BucketID:          b.ID,
		Bucket:            b.Name,
		Alias:             newAccessPointAlias(),
		NetworkOrigin:     origin,
		VPCID:             vpcID,
		Policy:            []byte(iamParam(r, "Policy")),
		PublicAccessBlock: []byte(iamParam(r, "PublicAccessBlockConfiguration")),
		CreatedAt:         time.Now().UTC(),
	}
	if err := s.Meta.CreateAccessPoint(r.Context(), ap); err != nil {
		if errors.Is(err, meta.ErrAccessPointAlreadyExists) {
			writeError(w, r, ErrAccessPointAlreadyOwnedByYou)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, createAccessPointResponse{
		XMLNS: accessPointXMLNS,
		Result: createAccessPointResult{
			AccessPointArn: accessPointArn(ap.Name),
			Alias:          ap.Alias,
		},
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) accessPointGet(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "Name")
	if name == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	ap, err := s.Meta.GetAccessPoint(r.Context(), name)
	if err != nil {
		if errors.Is(err, meta.ErrAccessPointNotFound) {
			writeError(w, r, ErrNoSuchAccessPoint)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	res := getAccessPointResult{
		Name:           ap.Name,
		Bucket:         ap.Bucket,
		NetworkOrigin:  ap.NetworkOrigin,
		CreationDate:   ap.CreatedAt.UTC().Format(time.RFC3339),
		Alias:          ap.Alias,
		AccessPointArn: accessPointArn(ap.Name),
	}
	if ap.NetworkOrigin == accessPointOriginVPC && ap.VPCID != "" {
		res.VpcConfiguration = &struct {
			VpcID string `xml:"VpcId"`
		}{VpcID: ap.VPCID}
	}
	writeXML(w, http.StatusOK, getAccessPointResponse{
		XMLNS:    accessPointXMLNS,
		Result:   res,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) accessPointDelete(w http.ResponseWriter, r *http.Request) {
	name := iamParam(r, "Name")
	if name == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	if err := s.Meta.DeleteAccessPoint(r.Context(), name); err != nil {
		if errors.Is(err, meta.ErrAccessPointNotFound) {
			writeError(w, r, ErrNoSuchAccessPoint)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, deleteAccessPointResponse{
		XMLNS:    accessPointXMLNS,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}

func (s *Server) accessPointList(w http.ResponseWriter, r *http.Request) {
	bucket := iamParam(r, "Bucket")
	bucketID := uuid.Nil
	if bucket != "" {
		b, err := s.Meta.GetBucket(r.Context(), bucket)
		if err != nil {
			mapMetaErr(w, r, err)
			return
		}
		bucketID = b.ID
	}
	list, err := s.Meta.ListAccessPoints(r.Context(), bucketID)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	resp := listAccessPointsResponse{
		XMLNS:    accessPointXMLNS,
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	}
	for _, ap := range list {
		resp.Result.AccessPointList.AccessPoint = append(
			resp.Result.AccessPointList.AccessPoint,
			accessPointEntryFromMeta(ap),
		)
	}
	writeXML(w, http.StatusOK, resp)
}
