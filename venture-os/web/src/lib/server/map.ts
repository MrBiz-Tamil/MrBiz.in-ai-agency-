import type { ApprovalSummary } from '$lib/gen/venture/approval/v1/approval_pb';
import type { BusinessHealth } from '$lib/gen/venture/portfolio/v1/portfolio_pb';
import type { Step } from '$lib/gen/venture/provisioner/v1/provisioner_pb';
import type { ApprovalView, BusinessView, StepView } from '$lib/types';

export function businessView(h: BusinessHealth): BusinessView {
	return {
		id: h.tenantId,
		slug: h.slug,
		name: h.name,
		status: h.status,
		role: h.role,
		currency: h.baseCurrency,
		hasForecast: h.forecastAvailable,
		cash: h.cashMinor,
		minCash: h.minCashMinor90d,
		cashNegative: h.cashNegativeWithin90d,
		runwayDays: h.runwayDays,
		pending: h.pendingApprovals,
		highRisk: h.highRiskPending,
		lastRun: h.lastProvisionStatus
	};
}

export function approvalView(a: ApprovalSummary): ApprovalView {
	return {
		tenantId: a.tenantId,
		business: a.businessName,
		id: a.approvalId,
		kind: a.kind,
		status: a.status,
		title: a.title,
		summary: a.summary,
		requestedBy: a.requestedBy,
		risk: a.riskLevel,
		amount: a.currency ? a.amountMinor : null,
		currency: a.currency,
		createdAt: a.createdAt
	};
}

export function stepView(s: Step): StepView {
	return { key: s.key, status: s.status, attempts: s.attempts, error: s.error };
}
