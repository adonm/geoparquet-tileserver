package tiles

import (
	"net/url"
	"strings"
)

// ProxyRoute maps an upstream URL prefix to a pgvs3 bucket.
type ProxyRoute struct {
	Prefix string
	Bucket string
}

var (
	proxyBase   string
	proxyRoutes []ProxyRoute
)

// ConfigureSourceProxy rewrites matching upstream HTTP URLs to the local
// pgvs3 object-store endpoint. Call once at startup.
func ConfigureSourceProxy(base string, routes []ProxyRoute) {
	proxyBase = strings.TrimRight(base, "/")
	proxyRoutes = append([]ProxyRoute(nil), routes...)
}

// ProxyURL returns the pgvs3 URL for an upstream asset, or raw when no route
// matches or no proxy is configured.
func ProxyURL(raw string) string {
	if proxyBase == "" || len(proxyRoutes) == 0 {
		return raw
	}
	for _, route := range proxyRoutes {
		if !strings.HasPrefix(raw, route.Prefix) {
			continue
		}
		rest := strings.TrimPrefix(raw, route.Prefix)
		rest = strings.TrimPrefix(rest, "/")
		pathPart, query, _ := strings.Cut(rest, "?")
		u, err := url.Parse(proxyBase + "/" + route.Bucket + "/" + pathPart)
		if err != nil {
			return raw
		}
		if query != "" {
			u.RawQuery = query
		}
		return u.String()
	}
	return raw
}
