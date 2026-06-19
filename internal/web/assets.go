package web

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

var (
	assetHashMu sync.Mutex
	assetHashes = map[string]string{}
)

// assetURL returns a cache-busting URL for an embedded static asset, e.g.
// "/static/output.css?v=1a2b3c4d5e". The version is a short content hash
// computed once per asset from the embedded FS, so a new build (changed bytes)
// yields a new URL and browsers re-fetch the file — while the long-lived
// immutable Cache-Control stays correct because the URL is content-addressed.
//
// Without this, output.css is served immutable at a fixed path and browsers
// keep a stale stylesheet across deploys (icons render at the wrong size, the
// sr-only skip link shows, etc.). Registered as the "asset" template func.
func assetURL(name string) string {
	assetHashMu.Lock()
	defer assetHashMu.Unlock()
	v, ok := assetHashes[name]
	if !ok {
		if data, err := content.ReadFile("static/" + name); err == nil {
			sum := sha256.Sum256(data)
			v = hex.EncodeToString(sum[:])[:10]
		}
		assetHashes[name] = v
	}
	if v == "" {
		return "/static/" + name
	}
	return "/static/" + name + "?v=" + v
}
