// Blueprint JSON as streamed by the Genesis interview, plus helpers to present it. The server validates every
// blueprint; these types describe what the preview may rely on and the helpers tolerate absent sections.

export interface Account {
	code: string;
	name: string;
	type: string;
	parent_code?: string;
	postable: boolean;
}
export interface Stage {
	key: string;
	name: string;
	win_probability_bps: number;
}
export interface Pipeline {
	key: string;
	name: string;
	entity_type: string;
	stages: Stage[];
	custom_fields?: { key: string; label: string; type: string; required: boolean }[];
}
export interface PricingRule {
	name: string;
	model: string;
	currency: string;
	margin_floor_bps: number;
	margin_target_bps?: number;
	approval_threshold_minor?: number;
}
export interface Agent {
	key: string;
	role_title: string;
	reports_to?: string;
	responsibilities: string[];
	tools: string[];
	monthly_budget_minor: number;
}
export interface Blueprint {
	business: {
		name: string;
		slug: string;
		industry: string;
		country: string;
		base_currency: string;
		timezone: string;
		description: string;
		target_customers: string[];
		offerings: { name: string; kind: string }[];
		starting_capital_minor: number;
	};
	accounting: {
		chart_of_accounts: Account[];
		tax: { regime: string; prices_include_tax: boolean; rates: { code: string; name: string; rate_bps: number }[] };
	};
	crm: { pipelines: Pipeline[] };
	pricing: PricingRule[];
	support: { channels: { kind: string; name: string; route_to_team: string }[]; kb_docs: { slug: string; title: string }[] };
	agents: Agent[];
	workflows: { key: string; name: string; trigger: string; steps: string[]; requires_approval: boolean }[];
}

/** Parses the draft bytes from the control plane. Returns null for empty or malformed input. */
export function parseBlueprint(bytes: Uint8Array | string | undefined | null): Blueprint | null {
	if (!bytes || (typeof bytes !== 'string' && bytes.length === 0)) return null;
	try {
		const text = typeof bytes === 'string' ? bytes : new TextDecoder().decode(bytes);
		const v = JSON.parse(text);
		return v && typeof v === 'object' && v.business ? (v as Blueprint) : null;
	} catch {
		return null;
	}
}

export interface OrgNode {
	agent: Agent;
	reports: OrgNode[];
}

/** Builds the org chart forest from reports_to links. Cycles and dangling links cannot loop forever. */
export function orgTree(agents: Agent[]): OrgNode[] {
	const byKey = new Map(agents.map((a) => [a.key, a]));
	const children = new Map<string, Agent[]>();
	const roots: Agent[] = [];
	for (const a of agents) {
		if (a.reports_to && byKey.has(a.reports_to) && a.reports_to !== a.key) {
			children.set(a.reports_to, [...(children.get(a.reports_to) ?? []), a]);
		} else {
			roots.push(a);
		}
	}
	const seen = new Set<string>();
	const build = (a: Agent): OrgNode => {
		seen.add(a.key);
		return { agent: a, reports: (children.get(a.key) ?? []).filter((c) => !seen.has(c.key)).map(build) };
	};
	return roots.map(build);
}

export interface AccountGroup {
	account: Account;
	children: Account[];
}

/** Groups accounts under their parent group for display. */
export function accountGroups(accounts: Account[]): AccountGroup[] {
	const groups = accounts.filter((a) => !a.parent_code).map((account) => ({ account, children: [] as Account[] }));
	const byCode = new Map(groups.map((g) => [g.account.code, g]));
	for (const a of accounts) if (a.parent_code) byCode.get(a.parent_code)?.children.push(a);
	return groups;
}
