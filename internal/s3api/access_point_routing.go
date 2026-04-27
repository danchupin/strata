package s3api

import (
	"context"
	"net"
	"strings"

	"github.com/danchupin/strata/internal/meta"
)

// extractAccessPointAlias inspects the request Host for the canonical AWS
// access-point shape `<alias>.s3-accesspoint.<region>.<host>`. The alias is
// returned (lowercased) when the second label is exactly `s3-accesspoint` and
// at least two further labels follow (region + host suffix). Empty return
// signals the caller should fall through to vhost / path-style routing.
func extractAccessPointAlias(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return "", false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 4 {
		return "", false
	}
	if labels[1] != "s3-accesspoint" {
		return "", false
	}
	alias := labels[0]
	if alias == "" {
		return "", false
	}
	return alias, true
}

// accessPointKey is the request-context key for the resolved access point.
// Stashed by the host-routing block in Server.ServeHTTP so downstream policy
// gates can apply the access-point's policy and network-origin restrictions.
type accessPointKey struct{}

func withAccessPoint(ctx context.Context, ap *meta.AccessPoint) context.Context {
	return context.WithValue(ctx, accessPointKey{}, ap)
}

// accessPointFromContext returns the resolved access point or nil if the
// request was not routed through one.
func accessPointFromContext(ctx context.Context) *meta.AccessPoint {
	if ctx == nil {
		return nil
	}
	ap, _ := ctx.Value(accessPointKey{}).(*meta.AccessPoint)
	return ap
}

// accessPointVPCHeader is the trusted VPC identifier the gateway operator
// injects (e.g. via an upstream load balancer) for VPC-origin access points.
// The value MUST come from a trusted edge — never echo a raw client header.
const accessPointVPCHeader = "X-Strata-VPC-ID"

// ExtractAccessPointAliasForTest exposes extractAccessPointAlias to package
// tests in s3api_test.
func ExtractAccessPointAliasForTest(host string) (string, bool) {
	return extractAccessPointAlias(host)
}
