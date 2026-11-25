package controlplane

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	portfoliov1 "github.com/mrbiz/venture-os/gen/venture/portfolio/v1"
	"github.com/mrbiz/venture-os/internal/auth"
	"github.com/mrbiz/venture-os/internal/financeengine"
)

type portfolioSvc struct{ *Server }

const portfolioHorizonDays = 90

// GetPortfolio returns one health row per business the caller belongs to. A failure to compute one business's
// numbers degrades that row (forecast_available=false) instead of failing the whole overview.
func (s *portfolioSvc) GetPortfolio(ctx context.Context, _ *connect.Request[portfoliov1.GetPortfolioRequest]) (*connect.Response[portfoliov1.GetPortfolioResponse], error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	ms, err := s.Repo.Memberships(ctx, p.Sub)
	if err != nil {
		return nil, s.internal("portfolio: memberships", err)
	}
	var approverTenants []uuid.UUID
	for _, m := range ms {
		if parseRole(m.Role) >= roleApprover {
			approverTenants = append(approverTenants, m.Tenant.ID)
		}
	}
	pending, err := s.Repo.PendingCounts(ctx, approverTenants)
	if err != nil {
		return nil, s.internal("portfolio: pending", err)
	}

	out := &portfoliov1.GetPortfolioResponse{}
	for _, m := range ms {
		h := &portfoliov1.BusinessHealth{
			TenantId: m.Tenant.ID.String(), Slug: m.Tenant.Slug, Name: m.Tenant.Name, Status: m.Tenant.Status,
			Role: m.Role, BaseCurrency: m.Tenant.BaseCurrency,
			PendingApprovals: int32(pending[m.Tenant.ID].Total), HighRiskPending: int32(pending[m.Tenant.ID].High),
		}
		if run, err := s.Repo.LatestRun(ctx, m.Tenant.ID); err == nil {
			h.LastProvisionStatus = string(run.Run.Status)
		}
		if m.Tenant.Status == "ACTIVE" {
			s.fillCash(ctx, m.Tenant.ID, h)
		}
		out.Businesses = append(out.Businesses, h)
	}
	return connect.NewResponse(out), nil
}

func (s *portfolioSvc) fillCash(ctx context.Context, tenant uuid.UUID, h *portfoliov1.BusinessHealth) {
	bp, err := s.Repo.ApprovedBlueprint(ctx, tenant)
	if err != nil {
		s.log().Error("portfolio: blueprint", "tenant", tenant, "err", err)
		return
	}
	opening, err := s.Repo.AccountBalance(ctx, tenant, bp.Blueprint.Accounting.SystemAccounts.Cash)
	if err != nil {
		s.log().Error("portfolio: cash", "tenant", tenant, "err", err)
		return
	}
	now := s.now()
	items, err := s.Repo.CashflowItems(ctx, tenant, now.AddDate(0, 0, portfolioHorizonDays))
	if err != nil {
		s.log().Error("portfolio: cashflow", "tenant", tenant, "err", err)
		return
	}
	f, err := financeengine.ForecastCash(opening, items, now, portfolioHorizonDays, financeengine.Options{})
	if err != nil {
		s.log().Error("portfolio: forecast", "tenant", tenant, "err", err)
		return
	}
	h.ForecastAvailable = true
	h.CashMinor = opening
	h.MinCashMinor_90D = f.MinClosingMinor
	h.CashNegativeWithin_90D = f.LowCash
	if f.RunwayDays != nil {
		h.RunwayDays = int32(*f.RunwayDays)
	}
}
