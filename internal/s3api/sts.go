package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"time"

	"github.com/danchupin/strata/internal/auth"
)

const stsXMLNS = "https://sts.amazonaws.com/doc/2011-06-15/"

type assumeRoleResponse struct {
	XMLName  xml.Name             `xml:"AssumeRoleResponse"`
	XMLNS    string               `xml:"xmlns,attr"`
	Result   assumeRoleResult     `xml:"AssumeRoleResult"`
	Metadata iamResponseMetadata  `xml:"ResponseMetadata"`
}

type assumeRoleResult struct {
	Credentials     stsCredentials     `xml:"Credentials"`
	AssumedRoleUser stsAssumedRoleUser `xml:"AssumedRoleUser"`
}

type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type stsAssumedRoleUser struct {
	AssumedRoleID string `xml:"AssumedRoleId"`
	Arn           string `xml:"Arn"`
}

func (s *Server) stsAssumeRole(w http.ResponseWriter, r *http.Request) {
	if s.STS == nil {
		writeError(w, r, ErrNotImplemented)
		return
	}
	roleArn := iamParam(r, "RoleArn")
	sessionName := iamParam(r, "RoleSessionName")
	if roleArn == "" || sessionName == "" {
		writeError(w, r, errIAMValidation)
		return
	}
	ttl := auth.DefaultSTSDuration
	if raw := iamParam(r, "DurationSeconds"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 900 || n > 43200 {
			writeError(w, r, errIAMValidation)
			return
		}
		ttl = time.Duration(n) * time.Second
	}
	owner := sessionName
	sess, err := s.STS.Issue(owner, ttl)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	writeXML(w, http.StatusOK, assumeRoleResponse{
		XMLNS: stsXMLNS,
		Result: assumeRoleResult{
			Credentials: stsCredentials{
				AccessKeyID:     sess.AccessKey,
				SecretAccessKey: sess.SecretKey,
				SessionToken:    sess.SessionToken,
				Expiration:      sess.Expiration.UTC().Format(time.RFC3339),
			},
			AssumedRoleUser: stsAssumedRoleUser{
				AssumedRoleID: sess.AccessKey + ":" + sessionName,
				Arn:           roleArn + "/" + sessionName,
			},
		},
		Metadata: iamResponseMetadata{RequestID: newRequestID()},
	})
}
