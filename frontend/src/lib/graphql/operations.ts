// GraphQL documents used by the UI. They mirror backend-core/pkg/graphql/schema.graphql.

export const TICKET_FIELDS = `
	fragment TicketFields on ExecutionTicket {
		id status actionType priority payload result createdAt updatedAt
		assignedAgent { id roleName }
		conflictDiff { entityType field systemA valueA systemB valueB }
	}`;

export const Q_TICKETS = `${TICKET_FIELDS}
	query Tickets($ws: ID!) { tickets(workspaceId: $ws, limit: 100) { ...TicketFields } }`;

export const S_TICKET_UPDATED = `${TICKET_FIELDS}
	subscription TicketUpdated($ws: ID!) { ticketUpdated(workspaceId: $ws) { ...TicketFields } }`;

export const M_APPROVE = `${TICKET_FIELDS}
	mutation Approve($input: ApproveTicketInput!) { approveTicket(input: $input) { ...TicketFields } }`;

export const M_REJECT = `${TICKET_FIELDS}
	mutation Reject($input: RejectTicketInput!) { rejectTicket(input: $input) { ...TicketFields } }`;

export const Q_XRAY = `
	query XRay($id: ID!) { decisionXRay(ticketId: $id) { id stepNumber toolName policyDecision outputSanitized latencyMs tokenCost createdAt } }`;

export const S_AUDIT = `
	subscription Audit($id: ID!) { auditLogAppended(ticketId: $id) { id stepNumber toolName policyDecision outputSanitized latencyMs tokenCost createdAt } }`;

export const AGENT_FIELDS = `
	fragment AgentFields on AgentDefinition { id roleName modelTier heartbeatCron dailyBudgetUsd spentTodayUsd status }`;

export const Q_ROSTER = `${AGENT_FIELDS}
	query Roster($ws: ID!) { agentRoster(workspaceId: $ws) { ...AgentFields } }`;

export const M_BUDGET = `${AGENT_FIELDS}
	mutation Budget($id: ID!, $usd: Decimal!) { updateAgentBudget(agentId: $id, dailyBudgetUsd: $usd) { ...AgentFields } }`;

export const M_PAUSE = `${AGENT_FIELDS}
	mutation Pause($id: ID!) { pauseAgent(agentId: $id) { ...AgentFields } }`;

export const M_RESUME = `${AGENT_FIELDS}
	mutation Resume($id: ID!) { resumeAgent(agentId: $id) { ...AgentFields } }`;

export const M_HEARTBEAT = `mutation Heartbeat($id: ID!) { triggerManualHeartbeat(agentId: $id) }`;

export const M_CORRECTION = `
	mutation Correct($input: AgentCorrectionInput!) { submitAgentCorrection(input: $input) }`;

export interface Ticket {
	id: string;
	status: 'QUEUED' | 'RUNNING' | 'PENDING_APPROVAL' | 'CONFLICT_HOLD' | 'COMPLETED' | 'FAILED';
	actionType: string;
	priority: number;
	payload: Record<string, any>;
	result: Record<string, any> | null;
	createdAt: string;
	updatedAt: string;
	assignedAgent: { id: string; roleName: string } | null;
	conflictDiff: { entityType: string; field: string; systemA: string; valueA: string; systemB: string; valueB: string } | null;
}

export interface Agent {
	id: string;
	roleName: string;
	modelTier: 'FLASH' | 'FRONTIER';
	heartbeatCron: string | null;
	dailyBudgetUsd: string;
	spentTodayUsd: string;
	status: string;
}

export interface AuditLog {
	id: string;
	stepNumber: number;
	toolName: string | null;
	policyDecision: string;
	outputSanitized: Record<string, any> | null;
	latencyMs: number;
	tokenCost: string;
	createdAt: string;
}

/** How a closed ticket ended, derived from the stored result. */
export function resolution(t: Ticket): 'touchless' | 'human_approved' | 'rejected' | 'failed' | null {
	if (t.status === 'COMPLETED') return t.result?.approved_by ? 'human_approved' : 'touchless';
	if (t.status === 'FAILED') return t.result?.rejected_by ? 'rejected' : 'failed';
	return null;
}

/** Policy details recorded when a ticket was held for approval. */
export function policyOf(t: Ticket): { rule: string; reason: string; assignedRole: string } | null {
	const p = t.result?.policy;
	return p && typeof p === 'object' ? (p as any) : null;
}

/** First error message of a urql result, with a friendly fallback. */
export function errorMessage(error: { graphQLErrors?: { message: string }[]; message?: string } | undefined): string | null {
	if (!error) return null;
	return error.graphQLErrors?.[0]?.message ?? 'Something went wrong. Please try again.';
}
