// Turns an approval's raw payload into something a founder can review at a glance. The server computed every
// number; this only arranges them. Unknown shapes fall back to a labelled key/value listing, never to guessing.

export interface Fact {
	label: string;
	value: string;
	/** Money facts carry minor units so the view can format them in the right currency. */
	money?: { minor: bigint; currency: string };
	tone?: 'bad' | 'warn' | 'good';
}

export interface DocLine {
	description: string;
	quantity: number;
	unitMinor: bigint;
	totalMinor: bigint;
}

export interface PayloadView {
	kind: 'quote' | 'document' | 'blocker' | 'generic';
	facts: Fact[];
	components?: { name: string; minor: bigint }[];
	lines?: DocLine[];
	totals?: { subtotal: bigint; tax: bigint; total: bigint; taxRateBps: number; currency: string };
	text?: string;
	flags: string[];
	raw: unknown;
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null && !Array.isArray(v);
const big = (v: unknown): bigint => {
	try {
		return typeof v === 'bigint' ? v : BigInt(Math.trunc(Number(v ?? 0)));
	} catch {
		return 0n;
	}
};
const str = (v: unknown): string => (typeof v === 'string' ? v : v == null ? '' : String(v));

export function parsePayload(bytes: Uint8Array | string | undefined, kind: string, currency: string): PayloadView {
	let raw: unknown = null;
	try {
		raw = bytes ? JSON.parse(typeof bytes === 'string' ? bytes : new TextDecoder().decode(bytes)) : null;
	} catch {
		/* shown as generic */
	}
	if (!isObj(raw)) return { kind: 'generic', facts: [], flags: [], raw };

	if (kind === 'QUOTE' && isObj(raw.result)) {
		const r = raw.result;
		const cur = str(r.currency) || currency;
		const facts: Fact[] = [
			{ label: 'Customer', value: str(raw.customer) },
			{ label: 'Quoted price', value: '', money: { minor: big(r.price_minor), currency: cur } },
			{ label: 'Total cost', value: '', money: { minor: big(r.cost_minor), currency: cur } },
			{ label: 'Margin', value: `${(Number(r.margin_bps) / 100).toFixed(2)}%`, tone: r.below_floor ? 'bad' : 'good' },
			{ label: 'Margin floor', value: `${(Number(r.margin_floor_bps) / 100).toFixed(2)}%` },
			{ label: 'Valid for', value: `${str(raw.valid_days)} days` }
		];
		const comps = Array.isArray(r.components) ? r.components.filter(isObj).map((c) => ({ name: str(c.name), minor: big(c.amount_minor) })) : [];
		return {
			kind: 'quote',
			facts,
			components: comps,
			flags: Array.isArray(r.approval_reasons) ? r.approval_reasons.map(str) : [],
			text: str(raw.note) || undefined,
			raw
		};
	}

	if ((kind === 'INVOICE' || kind === 'PURCHASE_ORDER') && isObj(raw.totals)) {
		const t = raw.totals;
		const cur = str(raw.currency) || currency;
		const lines: DocLine[] = Array.isArray(t.lines)
			? t.lines.filter(isObj).map((l) => ({ description: str(l.description), quantity: Number(l.quantity), unitMinor: big(l.unit_price_minor), totalMinor: big(l.total_minor) }))
			: [];
		return {
			kind: 'document',
			facts: [
				{ label: kind === 'INVOICE' ? 'Customer' : 'Supplier', value: str(raw.counterparty) },
				{ label: 'Reference', value: str(raw.reference) || '—' },
				{ label: 'Payment terms', value: `${str(raw.due_in_days)} days` },
				{ label: 'Tax code', value: str(raw.tax_code) }
			],
			lines,
			totals: { subtotal: big(t.subtotal_minor), tax: big(t.tax_minor), total: big(t.total_minor), taxRateBps: Number(t.tax_rate_bps ?? 0), currency: cur },
			flags: [],
			raw
		};
	}

	// A meeting request: show who will be emailed or told, which is exactly what the approver is deciding.
	if (kind === 'MEETING') {
		const facts = meetingFacts(raw);
		if (facts) return { kind: 'generic', facts, flags: [], raw };
	}

	if (raw.type === 'blocker') {
		return { kind: 'blocker', facts: [], text: str(raw.details), flags: [], raw };
	}

	// Generic: show scalar entries as facts, with money-looking keys formatted in the approval's currency.
	const facts: Fact[] = Object.entries(raw)
		.filter(([, v]) => ['string', 'number', 'boolean'].includes(typeof v))
		.map(([k, v]) =>
			/_minor$/.test(k) && typeof v === 'number'
				? { label: k.replace(/_minor$/, '').replace(/_/g, ' '), value: '', money: { minor: big(v), currency } }
				: { label: k.replace(/_/g, ' '), value: String(v) }
		);
	return { kind: 'generic', facts, flags: [], raw };
}

export const RISK_ORDER: Record<string, number> = { HIGH: 0, MEDIUM: 1, LOW: 2 };

/** True when approving this should make the founder pause: a payment, or anything the engine flagged. */
export function needsExtraCare(kind: string, risk: string, flags: string[]): boolean {
	return kind === 'PAYMENT' || risk === 'HIGH' || flags.length > 0;
}

const list = (v: unknown): string => (Array.isArray(v) ? v.map(str).join(', ') : '');

/**
 * The facts of a MEETING approval payload ({action:"schedule", meeting:{...}} or {action:"cancel", cancel:{...}}), or
 * null for a shape we do not recognise (which then falls back to the generic listing). Attendees are shown in full:
 * they are the people who would be emailed.
 */
export function meetingFacts(raw: Obj): Fact[] | null {
	if (raw.action === 'schedule' && isObj(raw.meeting)) {
		const m = raw.meeting;
		return [
			{ label: 'Action', value: 'Create a meeting' },
			{ label: 'Where', value: str(m.provider) === 'google' ? 'Google Calendar with a Google Meet link' : str(m.provider) },
			{ label: 'Title', value: str(m.title) },
			{ label: 'Starts', value: `${str(m.start)}${m.timezone ? ` (${str(m.timezone)})` : ''}` },
			{ label: 'Length', value: `${str(m.duration_minutes)} minutes` },
			{ label: 'Will be emailed an invitation', value: list(m.attendees) || 'nobody', tone: Array.isArray(m.attendees) && m.attendees.length && m.notify ? 'warn' : undefined }
		];
	}
	if (raw.action === 'cancel' && isObj(raw.cancel)) {
		const c = raw.cancel;
		return [
			{ label: 'Action', value: 'Cancel a meeting' },
			{ label: 'Meeting', value: str(raw.title) || str(c.meeting_id) },
			{ label: 'Where', value: str(c.provider) },
			{ label: 'Attendees are told', value: c.notify ? 'yes, they are emailed a cancellation' : 'no', tone: c.notify ? 'warn' : undefined }
		];
	}
	return null;
}
