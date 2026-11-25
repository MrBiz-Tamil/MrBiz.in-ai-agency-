// Plain view models passed from server loads to components. They contain only strings, numbers, booleans and
// bigints, so SvelteKit can serialise them (protobuf message instances cannot be).

export interface BusinessView {
	id: string;
	slug: string;
	name: string;
	status: string;
	role: string;
	currency: string;
	hasForecast: boolean;
	cash: bigint;
	minCash: bigint;
	cashNegative: boolean;
	runwayDays: number;
	pending: number;
	highRisk: number;
	lastRun: string;
}

export interface ApprovalView {
	tenantId: string;
	business: string;
	id: string;
	kind: string;
	status: string;
	title: string;
	summary: string;
	requestedBy: string;
	risk: string;
	amount: bigint | null;
	currency: string;
	createdAt: string;
}

export interface StepView {
	key: string;
	status: string;
	attempts: number;
	error: string;
}

export const STATUS_TONE: Record<string, 'good' | 'warn' | 'bad' | 'neutral'> = {
	ACTIVE: 'good',
	EXECUTED: 'good',
	SUCCEEDED: 'good',
	APPROVED: 'good',
	PROVISIONING: 'warn',
	PENDING_APPROVAL: 'warn',
	EXECUTING: 'warn',
	DRAFT: 'neutral',
	SUSPENDED: 'bad',
	REJECTED: 'bad',
	FAILED: 'bad',
	ROLLED_BACK: 'bad',
	COMPENSATION_FAILED: 'bad',
	EXPIRED: 'neutral',
	CANCELLED: 'neutral',
	ARCHIVED: 'neutral'
};
