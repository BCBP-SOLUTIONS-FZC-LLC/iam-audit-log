// Package api embeds this service's machine-readable inbound event contract
// so the running binary can serve it (GET /asyncapi, GET /asyncapi.yaml)
// without the source tree at runtime (LLD §3, §7.3).
package api

import _ "embed"

// AsyncAPISpec is the verbatim contents of asyncapi.yaml (AsyncAPI 3.0,
// receive-only — AL-INV-10), embedded at compile time.
//
//go:embed asyncapi.yaml
var AsyncAPISpec []byte
