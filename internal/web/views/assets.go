package views

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
)

// datastarJS is the client library, compiled into the binary.
//
// ⚠ IT USED TO BE A <script src="https://cdn.jsdelivr.net/…">, and that was
// wrong for this product in three separate ways:
//
//   - The dashboard was DEAD in any deployment without egress to the public
//     internet — air-gapped, egress-filtered, or simply behind a proxy that does
//     not allow jsdelivr. A self-hosted single binary that needs a CDN to render
//     its own admin console is not self-hosted.
//   - It leaked the existence and usage of a private admin console to a third
//     party on every page load.
//   - It was a supply-chain hole on the highest-value page in the system. A
//     compromised or hijacked CDN path executes arbitrary script on the page
//     that MINTS API TOKENS and creates administrators.
//
// The bundle is pinned at the version below and its digest is asserted by a
// test, so an accidental or malicious substitution during a dependency update
// fails the build rather than shipping.
//
//go:embed assets/datastar.js
var datastarJS []byte

// DatastarVersion is the pinned upstream release, kept beside the bytes so the
// two cannot drift silently.
const DatastarVersion = "v1.0.2"

// DatastarSHA256 is the digest of the vendored bundle, recorded when it was
// fetched from
// https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js
// on 2026-09-15.
//
// ⚠ This is the whole value of vendoring. Copying a remote script into a
// repository without recording what it was makes the file unverifiable — the
// next reader cannot tell a legitimate upgrade from a substitution. A test
// compares this to the embedded bytes.
const DatastarSHA256 = "2837d87acf6ee0ba8e4e63765926c25a98d63883b02f88be194a86b81d3fd24a"

// DatastarJS returns the client library bytes.
func DatastarJS() []byte { return datastarJS }

// stylesheetHref is the <link> target, carrying a digest so a browser picks up a
// changed stylesheet without a hard refresh.
//
// A function rather than a constant because templ WILL evaluate an expression in
// an attribute value — which is exactly the context the old `<style>{ css }`
// did not work in.
func stylesheetHref() string {
	return "/admin/assets/app.css?v=" + AppCSSDigest()
}

// DatastarDigest returns the SHA-256 of the embedded bundle, for the test that
// pins it against DatastarSHA256.
func DatastarDigest() string {
	sum := sha256.Sum256(datastarJS)
	return hex.EncodeToString(sum[:])
}
