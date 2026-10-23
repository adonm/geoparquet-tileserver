package tiles

import "testing"

func TestProxyURL(t *testing.T) {
	proxyBase = "http://127.0.0.1:8014"
	proxyRoutes = []ProxyRoute{{Prefix: "https://data.source.coop/", Bucket: "sourcecoop"}}
	defer func() { proxyBase, proxyRoutes = "", nil }()
	got := ProxyURL("https://data.source.coop/cboettig/x/data.parquet?token=1")
	want := "http://127.0.0.1:8014/sourcecoop/cboettig/x/data.parquet?token=1"
	if got != want {
		t.Fatalf("ProxyURL = %q, want %q", got, want)
	}
	if got := ProxyURL("https://other.example/x"); got != "https://other.example/x" {
		t.Fatalf("unmatched = %q", got)
	}
}
