//go:build integration

package fixtures

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
)

func registryID(name string) *gluetypes.RegistryId {
	return &gluetypes.RegistryId{RegistryName: aws.String(name)}
}
