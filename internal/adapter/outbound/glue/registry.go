package glue

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsglue "github.com/aws/aws-sdk-go-v2/service/glue"
)

// GetSchemaVersionAPI is the one Glue call the resolver needs.
type GetSchemaVersionAPI interface {
	GetSchemaVersion(ctx context.Context, in *awsglue.GetSchemaVersionInput, opts ...func(*awsglue.Options)) (*awsglue.GetSchemaVersionOutput, error)
}

// RegistryResolver resolves schema-version ids through glue:GetSchemaVersion
// (read-only; deploy/iam/policy.json). Compiled schemas are cached by the
// Codec, so each version is fetched once per process.
type RegistryResolver struct {
	api GetSchemaVersionAPI
}

// NewRegistryResolver wraps a Glue client.
func NewRegistryResolver(api GetSchemaVersionAPI) *RegistryResolver {
	return &RegistryResolver{api: api}
}

// SchemaDefinition implements SchemaResolver.
func (r *RegistryResolver) SchemaDefinition(ctx context.Context, id string) (string, error) {
	out, err := r.api.GetSchemaVersion(ctx, &awsglue.GetSchemaVersionInput{SchemaVersionId: aws.String(id)})
	if err != nil {
		return "", err
	}
	if out.SchemaDefinition == nil || *out.SchemaDefinition == "" {
		return "", errors.New("empty schema definition")
	}
	return *out.SchemaDefinition, nil
}
