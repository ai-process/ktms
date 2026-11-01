package server

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/ai-process/ktms/internal/apikeys"
)

func TestRestrictCallFromMetadata(t *testing.T) {
	for _, c := range []struct {
		v          string
		restricted bool
		code       codes.Code
	}{{"", false, codes.OK}, {"restricted", true, codes.OK}, {" RESTRICTED ", true, codes.OK}, {"unrestricted", false, codes.InvalidArgument}} {
		ctx := context.Background()
		if c.v != "" {
			ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(metadataKeyDataClassification, c.v))
		}
		out, err := restrictCall(ctx)
		if status.Code(err) != c.code || apikeys.RestrictedFrom(out) != c.restricted {
			t.Errorf("%q: %v %v", c.v, apikeys.RestrictedFrom(out), err)
		}
	}
}
