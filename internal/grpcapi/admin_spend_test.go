package grpcapi

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// Invalid spend limits are refused before any storage is touched.
func TestSpendLimitAdminValidation(t *testing.T) {
	s := NewAdminServer(nil, nil, func() { t.Error("onChange must not fire on a rejected mutation") })
	ctx := context.Background()

	bad := []*pb.SpendLimitSpec{
		nil,
		{SubjectType: "team", SubjectId: "t1", Period: "month", LimitNanousd: 1},
		{SubjectType: "org", SubjectId: "x", Period: "day", LimitNanousd: 1},
		{SubjectType: "key", SubjectId: "", Period: "day", LimitNanousd: 1},
		{SubjectType: "key", SubjectId: "k", Period: "week", LimitNanousd: 1},
		{SubjectType: "key", SubjectId: "k", Period: "day", LimitNanousd: 0},
		{SubjectType: "user", SubjectId: "u", Period: "month", LimitNanousd: -5},
	}
	for _, spec := range bad {
		_, err := s.SetSpendLimit(ctx, &pb.SetSpendLimitRequest{SpendLimit: spec})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("SetSpendLimit(%v) = %v, want InvalidArgument", spec, err)
		}
	}

	_, err := s.DeleteSpendLimit(ctx, &pb.DeleteSpendLimitRequest{SubjectType: "key", SubjectId: "k", Period: "year"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("DeleteSpendLimit with bad period = %v, want InvalidArgument", err)
	}
}
