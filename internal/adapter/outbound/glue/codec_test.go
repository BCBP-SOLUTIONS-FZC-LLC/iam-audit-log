package glue

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"strings"
	"testing"

	awsglue "github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/google/uuid"
)

const userDeletedSchema = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
 "required":["user_id"],"properties":{"user_id":{"type":"string"}},"additionalProperties":false}`

func frame(t *testing.T, version string, compression byte, payload []byte) []byte {
	t.Helper()
	id := uuid.MustParse(version)
	return append(append([]byte{headerVersion, compression}, id[:]...), payload...)
}

type countingResolver struct {
	defs  map[string]string
	calls int
}

func (r *countingResolver) SchemaDefinition(_ context.Context, id string) (string, error) {
	r.calls++
	d, ok := r.defs[id]
	if !ok {
		return "", errors.New("EntityNotFoundException")
	}
	return d, nil
}

const v1 = "b6f8f6d0-4b1a-4b1a-8b1a-1234567890ab"

// §7.3.1: header stripped; payload validated against the producer's
// registered schema version, compiled once per version.
func TestDecode_ValidatesAgainstRegisteredVersion(t *testing.T) {
	r := &countingResolver{defs: map[string]string{v1: userDeletedSchema}}
	c := NewCodec(r)
	for range 3 {
		out, err := c.Decode(context.Background(), "x", frame(t, v1, noCompression, []byte(`{"user_id":"u-1"}`)))
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `{"user_id":"u-1"}` {
			t.Fatalf("out = %s", out)
		}
	}
	if r.calls != 1 {
		t.Errorf("resolver called %d times; the compiled schema must be cached", r.calls)
	}
	if _, err := c.Decode(context.Background(), "x", frame(t, v1, noCompression, []byte(`{"user_id":7}`))); err == nil {
		t.Error("a payload that violates its registered schema must fail decode (→ DLQ)")
	}
}

// An unresolvable schema version is a decode failure (→ redelivery → DLQ),
// never a silent drop (§7.3.1).
func TestDecode_UnresolvableVersionFails(t *testing.T) {
	c := NewCodec(&countingResolver{})
	if _, err := c.Decode(context.Background(), "x", frame(t, uuid.NewString(), noCompression, []byte(`{}`))); err == nil {
		t.Fatal("expected error")
	}
	bad := NewCodec(&countingResolver{defs: map[string]string{v1: "not json"}})
	if _, err := bad.Decode(context.Background(), "x", frame(t, v1, noCompression, []byte(`{}`))); err == nil {
		t.Fatal("expected error for a non-JSON registered definition")
	}
	uncompilable := NewCodec(&countingResolver{defs: map[string]string{v1: `{"type":7}`}})
	if _, err := uncompilable.Decode(context.Background(), "x", frame(t, v1, noCompression, []byte(`{}`))); err == nil {
		t.Fatal("expected error for an uncompilable schema")
	}
}

func TestDecode_ZlibAndHeaderErrors(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"a":1}`))
	_ = zw.Close()
	c := NewCodec(nil) // strip-only (dev/test)
	out, err := c.Decode(context.Background(), "x", frame(t, v1, zlibCompressed, buf.Bytes()))
	if err != nil || string(out) != `{"a":1}` {
		t.Fatalf("zlib: %s, %v", out, err)
	}

	bomb := bytes.Repeat([]byte(" "), maxDecompressed+10)
	buf.Reset()
	zw = zlib.NewWriter(&buf)
	_, _ = zw.Write(append([]byte("{"), append(bomb, '}')...))
	_ = zw.Close()

	cases := map[string][]byte{
		"short":           {headerVersion, noCompression},
		"bad version":     append([]byte{0x02, noCompression}, make([]byte, 16)...),
		"bad compression": frame(t, v1, 0x09, []byte(`{}`)),
		"bad zlib":        frame(t, v1, zlibCompressed, []byte("not zlib")),
		"zlib bomb":       frame(t, v1, zlibCompressed, buf.Bytes()),
		"not json":        frame(t, v1, noCompression, []byte(`{nope`)),
	}
	for name, in := range cases {
		if _, err := c.Decode(context.Background(), "x", in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// AL-INV-10: the codec refuses to encode.
func TestEncode_Refuses_ALINV10(t *testing.T) {
	if _, _, err := NewCodec(nil).Encode(context.Background(), "UserDeleted", []byte(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "AL-INV-10") {
		t.Fatalf("err = %v", err)
	}
}

type fakeGlue struct {
	out *awsglue.GetSchemaVersionOutput
	err error
	got string
}

func (f *fakeGlue) GetSchemaVersion(_ context.Context, in *awsglue.GetSchemaVersionInput, _ ...func(*awsglue.Options)) (*awsglue.GetSchemaVersionOutput, error) {
	f.got = *in.SchemaVersionId
	return f.out, f.err
}

func TestRegistryResolver(t *testing.T) {
	def := userDeletedSchema
	f := &fakeGlue{out: &awsglue.GetSchemaVersionOutput{SchemaDefinition: &def}}
	got, err := NewRegistryResolver(f).SchemaDefinition(context.Background(), v1)
	if err != nil || got != def || f.got != v1 {
		t.Fatalf("got %q err %v id %s", got, err, f.got)
	}
	if _, err := NewRegistryResolver(&fakeGlue{out: &awsglue.GetSchemaVersionOutput{}}).SchemaDefinition(context.Background(), v1); err == nil {
		t.Error("empty definition must fail")
	}
	if _, err := NewRegistryResolver(&fakeGlue{err: errors.New("throttled")}).SchemaDefinition(context.Background(), v1); err == nil {
		t.Error("API error must fail")
	}
}
