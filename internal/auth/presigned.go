package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const presignSignatureParam = "X-Amz-Signature"

var ErrNotPresigned = errors.New("request has no presigned SigV4 query parameters")

func hasPresignedParams(r *http.Request) bool {
	return r.URL.Query().Get(presignSignatureParam) != ""
}

func parsePresigned(r *http.Request) (*parsedAuth, time.Time, int64, error) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != sigAlgorithm {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	credential := q.Get("X-Amz-Credential")
	credParts := strings.Split(credential, "/")
	if len(credParts) != 5 || credParts[4] != sigTerminator {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	if signedHeaders == "" {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	signature := q.Get(presignSignatureParam)
	if signature == "" {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	reqDate := q.Get("X-Amz-Date")
	if reqDate == "" {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	t, err := time.Parse(sigTimeFormat, reqDate)
	if err != nil {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	expires, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires <= 0 || expires > 7*24*3600 {
		return nil, time.Time{}, 0, ErrMalformedAuth
	}
	return &parsedAuth{
		AccessKey:     credParts[0],
		Date:          credParts[1],
		Region:        credParts[2],
		Service:       credParts[3],
		SignedHeaders: strings.Split(signedHeaders, ";"),
		Signature:     signature,
	}, t, expires, nil
}

func canonicalQueryWithout(u *url.URL, exclude string) string {
	vals := u.Query()
	vals.Del(exclude)
	return canonicalQueryValues(vals)
}

func canonicalQueryValues(vals url.Values) string {
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sortStrings(keys)
	var b strings.Builder
	first := true
	for _, k := range keys {
		for _, v := range vals[k] {
			if !first {
				b.WriteByte('&')
			}
			first = false
			b.WriteString(uriEncode(k, true))
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

func canonicalRequestWithQuery(r *http.Request, query string, signedHeaders []string, hashedPayload string) string {
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(r.URL.EscapedPath()))
	b.WriteByte('\n')
	b.WriteString(query)
	b.WriteByte('\n')
	b.WriteString(canonicalHeaders(r, signedHeaders))
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(hashedPayload)
	return b.String()
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
