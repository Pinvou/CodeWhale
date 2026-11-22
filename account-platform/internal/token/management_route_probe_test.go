package token

import (
	"context"
)

type routeHealthProbeStub struct{}

func (routeHealthProbeStub) AdapterVersion() string { return "adapter-v1" }

func (routeHealthProbeStub) DiscoverModels(context.Context, string, []byte) ([]DiscoveredModel, error) {
	return []DiscoveredModel{{UpstreamModelRef: "route-health-model"}}, nil
}
