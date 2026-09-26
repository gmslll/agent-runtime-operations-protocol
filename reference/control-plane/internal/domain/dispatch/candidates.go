package dispatch

import (
	"context"
	"errors"
	"slices"

	"github.com/gmslll/agent-runtime-operations-protocol/reference/control-plane/internal/domain/registry"
)

type RegistryCandidateSource struct{ Registry *registry.Service }

func (source RegistryCandidateSource) Candidates(ctx context.Context, view RunView) ([]Candidate, error) {
	if source.Registry == nil || view.Validate() != nil {
		return nil, errors.New("registry candidate source unavailable")
	}
	snapshot, err := source.Registry.Snapshot(ctx, registry.DiscoveryQuery{TenantID: view.TenantID, AgentID: view.Agent.ID, AgentVersion: view.Agent.Version, SkillID: view.Agent.SkillID, ProtocolVersion: "1.0"})
	if err != nil {
		return nil, errors.New("registry discovery unavailable")
	}
	result := make([]Candidate, 0, len(snapshot.Instances))
	for _, instance := range snapshot.Instances {
		profile := ""
		for _, candidate := range []string{"direct", "proxy", "worker_pull"} {
			if slices.Contains(instance.Runtime.TransportProfiles, candidate) {
				profile = candidate
				break
			}
		}
		if profile == "" {
			continue
		}
		result = append(result, Candidate{InstanceID: instance.InstanceID, SessionID: instance.SessionID, ServiceID: instance.ServiceID, Generation: instance.Generation, ResourceVersion: instance.ResourceVersion, Priority: instance.Operator.Priority, Weight: instance.Operator.Weight, AvailableSlots: instance.Runtime.Capacity.AvailableSlots, TransportProfile: profile, Endpoint: instance.Endpoint.BaseURL, LeaseExpiresAt: instance.LeaseExpiresAt})
	}
	return result, nil
}
