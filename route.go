package main

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// rpcModelRouteRequest carries the route request plus the host_callback_id the
// executor will forward (to skip our own interceptor chain).
type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// routeModel implements model.route (§6).
func (p *plugin) routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	p.reloadIfNeeded()

	g, ok := p.resolveGroup(req.RequestedModel)
	if !ok || !g.Enabled {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}

	_ = pluginabi.MethodModelRoute // keep import used
	p.log.debug("route hit", map[string]any{"group": g.Name, "strategy": g.Strategy})
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "group:" + g.Name + ":" + g.Strategy,
	})
}
