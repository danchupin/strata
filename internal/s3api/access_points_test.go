package s3api_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

type apCreateResp struct {
	XMLName xml.Name `xml:"CreateAccessPointResponse"`
	Result  struct {
		AccessPointArn string `xml:"AccessPointArn"`
		Alias          string `xml:"Alias"`
	} `xml:"CreateAccessPointResult"`
}

type apGetResp struct {
	XMLName xml.Name `xml:"GetAccessPointResponse"`
	Result  struct {
		Name           string `xml:"Name"`
		Bucket         string `xml:"Bucket"`
		NetworkOrigin  string `xml:"NetworkOrigin"`
		Alias          string `xml:"Alias"`
		AccessPointArn string `xml:"AccessPointArn"`
		CreationDate   string `xml:"CreationDate"`
	} `xml:"GetAccessPointResult"`
}

type apListResp struct {
	XMLName xml.Name `xml:"ListAccessPointsResponse"`
	Result  struct {
		AccessPointList struct {
			AccessPoint []struct {
				Name           string `xml:"Name"`
				Bucket         string `xml:"Bucket"`
				NetworkOrigin  string `xml:"NetworkOrigin"`
				Alias          string `xml:"Alias"`
				AccessPointArn string `xml:"AccessPointArn"`
			} `xml:"AccessPoint"`
		} `xml:"AccessPointList"`
	} `xml:"ListAccessPointsResult"`
}

func TestAccessPoints_AnonymousDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "ListAccessPoints", "")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestAccessPoints_NonRootDenied(t *testing.T) {
	h := newHarness(t)
	resp := iamCall(t, h, "CreateAccessPoint", "alice", "Name", "ap-x", "Bucket", "bkt")
	h.mustStatus(resp, http.StatusForbidden)
}

func TestAccessPoints_RoundTripCRUD(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, root), http.StatusOK)

	// Create
	resp := iamCall(t, h, "CreateAccessPoint", root, "Name", "ap-one", "Bucket", "bkt")
	h.mustStatus(resp, http.StatusOK)
	var created apCreateResp
	decodeXML(t, resp.Body, &created)
	resp.Body.Close()
	if !strings.HasPrefix(created.Result.Alias, "ap-") || len(created.Result.Alias) != 15 {
		t.Fatalf("alias shape: %q", created.Result.Alias)
	}
	if !strings.Contains(created.Result.AccessPointArn, "accesspoint/ap-one") {
		t.Fatalf("arn: %q", created.Result.AccessPointArn)
	}

	// Get
	resp = iamCall(t, h, "GetAccessPoint", root, "Name", "ap-one")
	h.mustStatus(resp, http.StatusOK)
	var got apGetResp
	decodeXML(t, resp.Body, &got)
	resp.Body.Close()
	if got.Result.Name != "ap-one" || got.Result.Bucket != "bkt" {
		t.Fatalf("get round-trip: %+v", got.Result)
	}
	if got.Result.NetworkOrigin != "Internet" {
		t.Fatalf("origin: %q", got.Result.NetworkOrigin)
	}
	if got.Result.Alias != created.Result.Alias {
		t.Fatalf("alias mismatch: %q vs %q", got.Result.Alias, created.Result.Alias)
	}

	// List
	resp = iamCall(t, h, "ListAccessPoints", root)
	h.mustStatus(resp, http.StatusOK)
	var list apListResp
	decodeXML(t, resp.Body, &list)
	resp.Body.Close()
	if len(list.Result.AccessPointList.AccessPoint) != 1 || list.Result.AccessPointList.AccessPoint[0].Name != "ap-one" {
		t.Fatalf("list: %+v", list.Result.AccessPointList.AccessPoint)
	}

	// List filtered by bucket
	resp = iamCall(t, h, "ListAccessPoints", root, "Bucket", "bkt")
	h.mustStatus(resp, http.StatusOK)
	var list2 apListResp
	decodeXML(t, resp.Body, &list2)
	resp.Body.Close()
	if len(list2.Result.AccessPointList.AccessPoint) != 1 {
		t.Fatalf("list scoped: %+v", list2.Result.AccessPointList.AccessPoint)
	}

	// Delete
	resp = iamCall(t, h, "DeleteAccessPoint", root, "Name", "ap-one")
	h.mustStatus(resp, http.StatusOK)
	resp.Body.Close()

	// Get after delete -> NoSuchAccessPoint
	resp = iamCall(t, h, "GetAccessPoint", root, "Name", "ap-one")
	h.mustStatus(resp, http.StatusNotFound)
	body := h.readBody(resp)
	if !strings.Contains(body, "NoSuchAccessPoint") {
		t.Fatalf("get after delete body: %s", body)
	}
}

func TestAccessPoints_DoubleCreateConflict(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, root), http.StatusOK)

	resp := iamCall(t, h, "CreateAccessPoint", root, "Name", "ap-dup", "Bucket", "bkt")
	h.mustStatus(resp, http.StatusOK)
	resp.Body.Close()

	resp = iamCall(t, h, "CreateAccessPoint", root, "Name", "ap-dup", "Bucket", "bkt")
	h.mustStatus(resp, http.StatusConflict)
	body := h.readBody(resp)
	if !strings.Contains(body, "AccessPointAlreadyOwnedByYou") {
		t.Fatalf("dup body: %s", body)
	}
}

func TestAccessPoints_DeleteMissing(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "DeleteAccessPoint", root, "Name", "ap-missing")
	h.mustStatus(resp, http.StatusNotFound)
	body := h.readBody(resp)
	if !strings.Contains(body, "NoSuchAccessPoint") {
		t.Fatalf("delete missing body: %s", body)
	}
}

func TestAccessPoints_CreateBucketNotFound(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	resp := iamCall(t, h, "CreateAccessPoint", root, "Name", "ap-x", "Bucket", "noexist")
	h.mustStatus(resp, http.StatusNotFound)
	body := h.readBody(resp)
	if !strings.Contains(body, "NoSuchBucket") {
		t.Fatalf("body: %s", body)
	}
}

func TestAccessPoints_CreateVPC(t *testing.T) {
	h := newHarness(t)
	root := s3api.IAMRootPrincipal
	h.mustStatus(h.doString(http.MethodPut, "/bkt", "", testPrincipalHeader, root), http.StatusOK)

	resp := iamCall(t, h, "CreateAccessPoint", root,
		"Name", "ap-vpc", "Bucket", "bkt", "VpcConfiguration.VpcId", "vpc-abc123",
	)
	h.mustStatus(resp, http.StatusOK)
	resp.Body.Close()

	resp = iamCall(t, h, "GetAccessPoint", root, "Name", "ap-vpc")
	h.mustStatus(resp, http.StatusOK)
	body := h.readBody(resp)
	if !strings.Contains(body, "<NetworkOrigin>VPC</NetworkOrigin>") {
		t.Fatalf("origin not VPC: %s", body)
	}
	if !strings.Contains(body, "<VpcId>vpc-abc123</VpcId>") {
		t.Fatalf("vpc id missing: %s", body)
	}
}
