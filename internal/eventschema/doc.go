// Package eventschema holds one open JSON Schema per consumed AsyncAPI
// message, DERIVED from api/asyncapi.yaml's <Name>Payload schemas by
// `make extract-schemas` (schema-gov extract). Never edit them by hand; CI's
// `extract --check` fails on drift. They exist for the platform schema
// governance checks (schema-registry.yml `schema-gov validate`, D-24) only.
// The payload is producer-owned and validated on decode against the
// producer's Glue schema version (LLD §7.3.1), so these files are not
// embedded, and nothing here is registered in Glue (AL-INV-10).
package eventschema
