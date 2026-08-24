package catalogadmin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
)

// The operator's hand on the route's capability record.
//
// A route declares nothing about what it serves; the probe worker finds out.
// These two calls are the operator's way in: to ask for a probe now, and to
// write a verdict the worker will not overwrite -- the only way an endpoint
// that is never probed automatically gets published, and the only way to say
// "do not send this here" for an endpoint the upstream would accept.

// routeEndpointAllowed checks that the route belongs to the model and that the
// endpoint is one of the protocols its provider speaks. A row for any other
// endpoint would be invisible to every reader, which filters by the provider's
// protocols; refusing it here is the difference between "that did nothing" and
// a 200.
func (s *Service) routeEndpointsAllowed(
	ctx context.Context, modelID, routeID uuid.UUID, endpoints []string,
) error {
	row, err := s.q.RouteUnderModel(ctx, gwdb.RouteUnderModelParams{
		RouteID: pgID(routeID), ModelID: pgID(modelID),
	})
	if db.IsNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("catalogadmin: query the route: %w", err)
	}
	for _, endpoint := range endpoints {
		protocol, ok := catalog.ProtocolForEndpoint(endpoint)
		if !ok {
			return invalid("Unknown endpoint: %s", endpoint)
		}
		if !slices.Contains(row.ProviderProtocols, protocol) {
			return invalid("Endpoint %s belongs to the %s protocol and this route's provider only speaks %s, "+
				"so no request for it would ever reach this route.",
				endpoint, protocol, strings.Join(row.ProviderProtocols, ", "))
		}
	}
	return nil
}

// OverrideRouteProbe writes the operator's verdict for one endpoint, or clears
// it: `ok` and `unsupported` are recorded as the operator's and left alone by
// the worker; `unverified` hands the row back and asks for a probe.
func (s *Service) OverrideRouteProbe(
	ctx context.Context, modelID, routeID uuid.UUID, endpoint, status string,
) (routeprobe.Verdict, error) {
	if err := s.routeEndpointsAllowed(ctx, modelID, routeID, []string{endpoint}); err != nil {
		return routeprobe.Verdict{}, err
	}
	switch status {
	case routeprobe.StatusOK, routeprobe.StatusUnsupported:
		if err := s.probes.Override(ctx, pgID(routeID), endpoint, status); err != nil {
			return routeprobe.Verdict{}, err
		}
	case routeprobe.StatusUnverified:
		if err := s.probes.ClearOverride(ctx, pgID(routeID), endpoint); err != nil {
			return routeprobe.Verdict{}, err
		}
	default:
		return routeprobe.Verdict{}, invalid("status must be ok, unsupported or unverified")
	}
	s.invalidate(ctx)
	v, err := s.probes.Verdict(ctx, pgID(routeID), endpoint)
	if err != nil {
		return routeprobe.Verdict{}, err
	}
	return v, nil
}

// ProbeRoute asks for a probe of one route now, on the endpoints named or on
// every automatically probeable one. Naming an endpoint the provider does not
// speak is refused for the same reason as an override of it.
func (s *Service) ProbeRoute(ctx context.Context, modelID, routeID uuid.UUID, endpoints []string) error {
	if err := s.routeEndpointsAllowed(ctx, modelID, routeID, endpoints); err != nil {
		return err
	}
	return s.probes.Probe(ctx, pgID(routeID), endpoints)
}
