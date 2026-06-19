package web

import (
	"regexp"
	"strings"
	"testing"
)

func TestAssetURL(t *testing.T) {
	// Embedded asset → cache-busting ?v=<hash>; hash is stable across calls.
	got := assetURL("output.css")
	if !regexp.MustCompile(`^/static/output\.css\?v=[0-9a-f]{10}$`).MatchString(got) {
		t.Errorf("assetURL(output.css) = %q, want /static/output.css?v=<10 hex>", got)
	}
	if got != assetURL("output.css") {
		t.Error("assetURL should be deterministic for the same asset")
	}
	// Different assets get different versions.
	if assetURL("output.css") == assetURL("init.js") {
		t.Error("distinct assets should have distinct versions")
	}
	// Unknown asset → plain path, no ?v=.
	if u := assetURL("does-not-exist.js"); strings.Contains(u, "?v=") {
		t.Errorf("assetURL(missing) = %q, want no version", u)
	}
}
