package grpcapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func TestGetSpendReportsEachCappedWindow(t *testing.T) {
	s := NewAdminServer(nil, nil, nil)
	s.SetSpendLimitResolver(func(subject, id, period string) int64 {
		if subject == "key" && id == "billing" && period == "day" {
			return 5_000_000_000
		}
		return 0
	})
	s.SetSpendReader(func(_ context.Context, subject, id, period string) (int64, time.Time, error) {
		return 7_000_000_000, time.Now().Add(time.Hour), nil
	})
	resp, err := s.GetSpend(context.Background(), &pb.GetSpendRequest{SubjectType: "key", SubjectId: "billing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Windows) != 2 {
		t.Fatalf("windows = %v, want day and month", resp.Windows)
	}
	day, month := resp.Windows[0], resp.Windows[1]
	if day.Period != "day" || !day.Metered || day.LimitNanousd != 5_000_000_000 || day.SpentNanousd != 7_000_000_000 || day.RemainingNanousd != 0 {
		t.Fatalf("day = %v, want metered, overspent, nothing remaining", day)
	}
	if month.Period != "month" || month.Metered || month.LimitNanousd != 0 {
		t.Fatalf("month = %v, want uncapped and unmetered", month)
	}

	team, err := s.GetSpend(context.Background(), &pb.GetSpendRequest{SubjectType: "team", SubjectId: "t"})
	if err != nil || len(team.Windows) != 1 || team.Windows[0].Period != "month" {
		t.Fatalf("team = %v, %v; want the monthly window only", team, err)
	}

	for _, bad := range []*pb.GetSpendRequest{{SubjectType: "org", SubjectId: "x"}, {SubjectType: "key"}, {SubjectType: "key", SubjectId: "*"}} {
		if _, err := s.GetSpend(context.Background(), bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("GetSpend(%v) = %v, want InvalidArgument", bad, err)
		}
	}

	s.SetSpendReader(func(context.Context, string, string, string) (int64, time.Time, error) {
		return 0, time.Time{}, errors.New("redis down")
	})
	if _, err := s.GetSpend(context.Background(), &pb.GetSpendRequest{SubjectType: "key", SubjectId: "billing"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unreadable counters: %v, want Unavailable", err)
	}
}

// Without Redis the caps still show; only the counting stops.
func TestGetSpendWithoutRedisShowsTheCap(t *testing.T) {
	s := NewAdminServer(nil, nil, nil)
	s.SetSpendLimitResolver(func(string, string, string) int64 { return 1_000 })
	resp, err := s.GetSpend(context.Background(), &pb.GetSpendRequest{SubjectType: "user", SubjectId: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range resp.Windows {
		if w.LimitNanousd != 1_000 || w.Metered || w.SpentNanousd != 0 {
			t.Fatalf("window = %v, want the cap, unmetered", w)
		}
	}
}
