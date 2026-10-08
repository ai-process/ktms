package grpcapi

import (
	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

func effortToString(e pb.Effort) string {
	switch e {
	case pb.Effort_EFFORT_LOW:
		return registry.EffortLow
	case pb.Effort_EFFORT_MEDIUM:
		return registry.EffortMedium
	case pb.Effort_EFFORT_HIGH:
		return registry.EffortHigh
	default:
		return ""
	}
}

func effortToProto(e string) pb.Effort {
	switch e {
	case registry.EffortLow:
		return pb.Effort_EFFORT_LOW
	case registry.EffortMedium:
		return pb.Effort_EFFORT_MEDIUM
	case registry.EffortHigh:
		return pb.Effort_EFFORT_HIGH
	default:
		return pb.Effort_EFFORT_UNSPECIFIED
	}
}

func effortsToStrings(in []pb.Effort) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, effortToString(e))
	}
	return out
}

func effortsToProto(in []string) []pb.Effort {
	out := make([]pb.Effort, 0, len(in))
	for _, e := range in {
		out = append(out, effortToProto(e))
	}
	return out
}

func modelFromProto(m *pb.ModelSpec) *proxydb.Model {
	return &proxydb.Model{
		ID:                  m.GetId(),
		Vendor:              m.GetVendor(),
		Endpoint:            m.GetEndpoint(),
		Efforts:             effortsToStrings(m.GetEfforts()),
		Capabilities:        m.GetCapabilities(),
		RPM:                 m.GetRpm(),
		PriceInPerMtok:      m.GetPriceInPerMtok(),
		PriceOutPerMtok:     m.GetPriceOutPerMtok(),
		PriceInPeakPerMtok:   m.GetPriceInPeakPerMtok(),
		PriceOutPeakPerMtok:  m.GetPriceOutPeakPerMtok(),
		PriceInBatchPerMtok:  m.GetPriceInBatchPerMtok(),
		PriceOutBatchPerMtok: m.GetPriceOutBatchPerMtok(),
		DailyTokensPerKey:    m.GetDailyTokensPerKey(),
		DailyTokensPerUser:   m.GetDailyTokensPerUser(),
		Enabled:              m.GetEnabled(),
	}
}

func modelToProto(m *proxydb.Model) *pb.ModelSpec {
	return &pb.ModelSpec{
		Id:                   m.ID,
		Vendor:               m.Vendor,
		Endpoint:             m.Endpoint,
		Efforts:              effortsToProto(m.Efforts),
		Capabilities:         m.Capabilities,
		Rpm:                  m.RPM,
		PriceInPerMtok:       m.PriceInPerMtok,
		PriceOutPerMtok:      m.PriceOutPerMtok,
		PriceInPeakPerMtok:   m.PriceInPeakPerMtok,
		PriceOutPeakPerMtok:  m.PriceOutPeakPerMtok,
		PriceInBatchPerMtok:  m.PriceInBatchPerMtok,
		PriceOutBatchPerMtok: m.PriceOutBatchPerMtok,
		DailyTokensPerKey:    m.DailyTokensPerKey,
		DailyTokensPerUser:   m.DailyTokensPerUser,
		Enabled:              m.Enabled,
	}
}

func schemaFromProto(s *pb.ResponseSchema) *llm.ResponseSchema {
	if s == nil {
		return nil
	}
	return &llm.ResponseSchema{
		Type:       llm.SchemaPropertyType(s.GetType()),
		Properties: propertiesFromProto(s.GetProperties()),
		Items:      propertyFromProto(s.GetItems()),
		Required:   s.GetRequired(),
	}
}

func propertyFromProto(p *pb.SchemaProperty) *llm.SchemaProperty {
	if p == nil {
		return nil
	}
	return &llm.SchemaProperty{
		Type:        llm.SchemaPropertyType(p.GetType()),
		Description: p.GetDescription(),
		Properties:  propertiesFromProto(p.GetProperties()),
		Items:       propertyFromProto(p.GetItems()),
		Required:    p.GetRequired(),
		Nullable:    p.GetNullable(),
	}
}

func propertiesFromProto(in map[string]*pb.SchemaProperty) map[string]*llm.SchemaProperty {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*llm.SchemaProperty, len(in))
	for k, v := range in {
		out[k] = propertyFromProto(v)
	}
	return out
}

func ruleToProto(r *proxydb.Rule) *pb.RuleSpec {
	return &pb.RuleSpec{
		Name:       r.Name,
		Priority:   r.Priority,
		Effort:     effortToProto(r.Effort),
		Attributes: r.Attributes,
		Use:        r.Use,
		Enabled:    r.Enabled,
	}
}
