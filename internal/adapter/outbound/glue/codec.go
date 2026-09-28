// Package glue is the consumer-side AWS Glue Schema Registry decode codec
// (LLD §7.3.1, AL-D12). It is decode-only — this service produces nothing
// and registers no schemas (AL-INV-10).
//
// platform-events invokes a consumer codec only when the envelope's
// `dataschema` attribute is populated; a plain-JSON envelope (no Glue
// header) never reaches this codec. That is exactly AL-D12's
// probe-and-fallback rule — key on `dataschema`, never sniff bytes.
package glue

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Glue wire format: [0]=header version 0x03, [1]=compression, [2:18]=schema
// version UUID, then the payload.
const (
	headerVersion  byte = 0x03
	noCompression  byte = 0x00
	zlibCompressed byte = 0x05
	headerSize          = 18
	// maxDecompressed bounds a zlib payload (defense against a bomb).
	maxDecompressed = 1 << 20
)

// SchemaResolver returns the registered JSON Schema definition for a Glue
// schema-version id (glue:GetSchemaVersion — the only Glue permission this
// service holds besides GetSchemaByDefinition).
type SchemaResolver interface {
	SchemaDefinition(ctx context.Context, schemaVersionID string) (string, error)
}

// Codec is the decode-only events.Codec.
type Codec struct {
	resolver SchemaResolver // nil → strip only (dev/test, LLD §7.3.1)

	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
}

// NewCodec returns a codec. With a resolver, every payload is validated
// against the producer's registered schema version and an unresolvable
// version is a decode failure → redelivery → DLQ (never a silent drop).
func NewCodec(resolver SchemaResolver) *Codec {
	return &Codec{resolver: resolver, compiled: map[string]*jsonschema.Schema{}}
}

// Encode always fails: this service never publishes (AL-INV-10).
func (*Codec) Encode(_ context.Context, eventType string, _ json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	return nil, "", fmt.Errorf("glue codec: decode-only, refusing to encode %q (AL-INV-10)", eventType)
}

// Decode strips (and if needed decompresses) the Glue header, then
// validates the JSON payload against its registered schema version.
func (c *Codec) Decode(ctx context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	versionID, payload, err := parse(encoded)
	if err != nil {
		return nil, err
	}
	if !json.Valid(payload) {
		return nil, errors.New("glue codec: payload is not valid JSON")
	}
	if c.resolver == nil {
		return json.RawMessage(payload), nil
	}
	schema, err := c.schema(ctx, versionID)
	if err != nil {
		return nil, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("glue codec: decode payload: %w", err)
	}
	if err := schema.Validate(inst); err != nil {
		return nil, fmt.Errorf("glue codec: payload fails schema version %s: %s", versionID, violations(err))
	}
	return json.RawMessage(payload), nil
}

func parse(encoded []byte) (versionID string, payload []byte, err error) {
	if len(encoded) < headerSize {
		return "", nil, fmt.Errorf("glue codec: %d bytes is shorter than the %d-byte header", len(encoded), headerSize)
	}
	if encoded[0] != headerVersion {
		return "", nil, fmt.Errorf("glue codec: header version 0x%02x, want 0x%02x", encoded[0], headerVersion)
	}
	id, err := uuid.FromBytes(encoded[2:headerSize])
	if err != nil {
		return "", nil, fmt.Errorf("glue codec: schema version id: %w", err)
	}
	body := encoded[headerSize:]
	switch encoded[1] {
	case noCompression:
		return id.String(), body, nil
	case zlibCompressed:
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return "", nil, fmt.Errorf("glue codec: zlib: %w", err)
		}
		out, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
		if cerr := zr.Close(); err == nil && cerr != nil && len(out) <= maxDecompressed {
			err = cerr
		}
		if err != nil {
			return "", nil, fmt.Errorf("glue codec: zlib: %w", err)
		}
		if len(out) > maxDecompressed {
			return "", nil, errors.New("glue codec: decompressed payload exceeds 1 MiB")
		}
		return id.String(), out, nil
	default:
		return "", nil, fmt.Errorf("glue codec: unsupported compression byte 0x%02x", encoded[1])
	}
}

// schema compiles (once per process) the registered definition for id.
func (c *Codec) schema(ctx context.Context, id string) (*jsonschema.Schema, error) {
	c.mu.Lock()
	s, ok := c.compiled[id]
	c.mu.Unlock()
	if ok {
		return s, nil
	}
	def, err := c.resolver.SchemaDefinition(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("glue codec: resolve schema version %s: %w", id, err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(def))
	if err != nil {
		return nil, fmt.Errorf("glue codec: schema version %s is not JSON: %w", id, err)
	}
	comp := jsonschema.NewCompiler()
	url := "glue://" + id + ".json"
	if err := comp.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("glue codec: schema version %s: %w", id, err)
	}
	s, err = comp.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("glue codec: compile schema version %s: %w", id, err)
	}
	c.mu.Lock()
	c.compiled[id] = s
	c.mu.Unlock()
	return s, nil
}

// violations renders a validation failure as instance locations plus the
// failed schema keywords only. The validator's own messages quote payload
// values (e.g. a non-matching email), and this error reaches logs via the
// consumer's retry/DLQ path, so values must never appear in it (LLD §11 log
// hygiene: IDs only, never payload).
func violations(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return "invalid"
	}
	var out []string
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, "/"+strings.Join(e.InstanceLocation, "/")+": "+strings.Join(e.ErrorKind.KeywordPath(), "/"))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) > 10 {
		out = append(out[:10], fmt.Sprintf("… %d more", len(out)-10))
	}
	return strings.Join(out, "; ")
}
