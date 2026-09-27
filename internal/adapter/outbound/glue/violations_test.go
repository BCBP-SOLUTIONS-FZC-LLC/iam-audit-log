package glue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// piiSchema constrains fields a producer could fill with personal data, so a
// violation would make the validator quote the value.
const piiSchema = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
 "properties":{
   "email":{"type":"string","pattern":"^[a-z]+@corp\\.example$"},
   "role":{"enum":["admin","member"]},
   "profile":{"type":"object","properties":{"phone":{"type":"string","maxLength":3}}}
 }}`

const secretEmail = "secret@example.com"

func compile(t *testing.T, def string) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(def))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("s.json", doc); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile("s.json")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validate(t *testing.T, s *jsonschema.Schema, payload string) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Validate(inst)
	if err == nil {
		t.Fatalf("payload %s must violate the schema", payload)
	}
	return err
}

// LLD §11 log hygiene: the rendered violation names where and which keyword,
// never the offending value. The raw validator message does quote it (that
// is why violations() exists).
func TestViolations_PathsAndKeywordsNeverValues(t *testing.T) {
	s := compile(t, piiSchema)
	payload := `{"email":"` + secretEmail + `","role":"superuser-jane.doe","profile":{"phone":"+15550100"}}`
	err := validate(t, s, payload)
	if !strings.Contains(err.Error(), secretEmail) {
		t.Logf("note: the validator no longer quotes values (%v); the sanitizer still must not", err)
	}
	got := violations(err)
	for _, secret := range []string{secretEmail, "superuser-jane.doe", "+15550100"} {
		if strings.Contains(got, secret) {
			t.Errorf("violations() leaks %q: %s", secret, got)
		}
	}
	for _, want := range []string{"/email: pattern", "/role: enum", "/profile/phone: maxLength"} {
		if !strings.Contains(got, want) {
			t.Errorf("violations() = %q, want it to contain %q", got, want)
		}
	}
}

// A non-ValidationError renders as "invalid" (no cause text at all).
func TestViolations_NonValidationError(t *testing.T) {
	if got := violations(errors.New("boom " + secretEmail)); got != "invalid" {
		t.Errorf("violations(plain) = %q", got)
	}
}

// More than 10 leaf violations are truncated with a count.
func TestViolations_Truncated(t *testing.T) {
	var props, vals []string
	for i := range 14 {
		props = append(props, fmt.Sprintf(`"f%02d":{"type":"integer"}`, i))
		vals = append(vals, fmt.Sprintf(`"f%02d":"v%02d"`, i, i))
	}
	s := compile(t, `{"type":"object","properties":{`+strings.Join(props, ",")+`}}`)
	got := violations(validate(t, s, `{`+strings.Join(vals, ",")+`}`))
	if n := strings.Count(got, ": type"); n != 10 {
		t.Errorf("leaf count = %d, want 10: %s", n, got)
	}
	if !strings.HasSuffix(got, "… 4 more") {
		t.Errorf("violations() = %q, want a '… 4 more' suffix", got)
	}
}

// End to end: the error Decode returns for a violating payload (which the
// consumer's retry/DLQ path logs) never contains the value.
func TestDecode_ViolationErrorHasNoPayloadValues(t *testing.T) {
	c := NewCodec(&countingResolver{defs: map[string]string{v1: piiSchema}})
	_, err := c.Decode(context.Background(), "x", frame(t, v1, noCompression, []byte(`{"email":"`+secretEmail+`","role":"root"}`)))
	if err == nil {
		t.Fatal("violating payload must fail decode")
	}
	msg := err.Error()
	if strings.Contains(msg, secretEmail) || strings.Contains(msg, `"root"`) || strings.Contains(msg, " root") {
		t.Errorf("decode error leaks payload values: %s", msg)
	}
	if !strings.Contains(msg, v1) || !strings.Contains(msg, "/email: pattern") || !strings.Contains(msg, "/role: enum") {
		t.Errorf("decode error = %q, want the version and paths", msg)
	}
}
