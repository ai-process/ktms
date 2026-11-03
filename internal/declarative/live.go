package declarative

import (
	"context"
	"fmt"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// State is Keystone's live configuration as the Admin API reports it.
type State struct {
	Config *pb.GetConfigResponse
	Keys   []*pb.APIKeyInfo
	Orgs   []*pb.Organization
	Teams  []*pb.Team

	NodeSchedules    []*pb.NodeScheduleInfo
	NodeSchedulesErr error // set when this Keystone cannot list them (it predates node schedules)
}

// Fetch reads the live configuration. It needs a key that may call GetConfig
// and the List methods.
func Fetch(ctx context.Context, c pb.AdminServiceClient) (*State, error) {
	cfg, err := c.GetConfig(ctx, &pb.GetConfigRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetConfig: %w", err)
	}
	keys, err := c.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListAPIKeys: %w", err)
	}
	orgs, err := c.ListOrganizations(ctx, &pb.ListOrganizationsRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListOrganizations: %w", err)
	}
	teams, err := c.ListTeams(ctx, &pb.ListTeamsRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListTeams: %w", err)
	}
	st := &State{Config: cfg, Keys: keys.GetKeys(), Orgs: orgs.GetOrganizations(), Teams: teams.GetTeams()}
	// Only a file with node_schedules needs them, so an older Keystone still plans the rest.
	if ns, err := c.ListNodeSchedules(ctx, &pb.ListNodeSchedulesRequest{}); err != nil {
		st.NodeSchedulesErr = fmt.Errorf("ListNodeSchedules: %w", err)
	} else {
		st.NodeSchedules = ns.GetSchedules()
	}
	return st, nil
}

func (s *State) orgByName(name string) *pb.Organization {
	for _, o := range s.Orgs {
		if o.GetName() == name {
			return o
		}
	}
	return nil
}

func (s *State) orgByID(id string) *pb.Organization {
	for _, o := range s.Orgs {
		if o.GetId() == id {
			return o
		}
	}
	return nil
}

func (s *State) team(orgID, name string) *pb.Team {
	for _, t := range s.Teams {
		if t.GetOrgId() == orgID && t.GetName() == name {
			return t
		}
	}
	return nil
}

func (s *State) teamByID(id string) *pb.Team {
	for _, t := range s.Teams {
		if t.GetId() == id {
			return t
		}
	}
	return nil
}

func (s *State) key(name string) *pb.APIKeyInfo {
	for _, k := range s.Keys {
		if k.GetName() == name {
			return k
		}
	}
	return nil
}
