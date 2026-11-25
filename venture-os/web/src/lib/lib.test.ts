import { describe, expect, it } from 'vitest';
import { RISK_ORDER, needsExtraCare, parsePayload } from './approvals';
import { accountGroups, orgTree, parseBlueprint, type Agent } from './blueprint';
import { formatBps, formatMoney, minorDigits, minorToDecimal, timeAgo } from './format';
import { rank, score, type PaletteItem } from './palette';

describe('money formatting is exact', () => {
	it('converts minor units to decimals without floats', () => {
		expect(minorToDecimal(1180562n, 2)).toBe('11805.62');
		expect(minorToDecimal(5n, 2)).toBe('0.05');
		expect(minorToDecimal(0n, 2)).toBe('0.00');
		expect(minorToDecimal(-1234567n, 2)).toBe('-12345.67');
		expect(minorToDecimal(123456n, 0)).toBe('123456');
		expect(minorToDecimal(1500n, 3)).toBe('1.500');
	});

	it('keeps precision beyond 2^53', () => {
		// 9,007,199,254,740,993 is not representable as a double; it must survive formatting.
		const out = formatMoney(900719925474099301n, 'USD', 'en-US');
		expect(out).toBe('$9,007,199,254,740,993.01');
	});

	it('knows currency minor digits', () => {
		expect(minorDigits('USD')).toBe(2);
		expect(minorDigits('INR')).toBe(2);
		expect(minorDigits('JPY')).toBe(0);
		expect(minorDigits('KWD')).toBe(3);
	});

	it('formats with locale grouping', () => {
		expect(formatMoney(500000000n, 'INR', 'en-IN')).toBe('₹50,00,000.00');
		expect(formatMoney(1112196n, 'USD', 'en-US')).toBe('$11,121.96');
		expect(formatMoney(123456n, 'JPY', 'en-US')).toBe('¥123,456');
		expect(formatMoney(-50000n, 'USD', 'en-US')).toBe('-$500.00');
	});

	it('formats basis points', () => {
		expect(formatBps(1200)).toBe('12.00%');
		expect(formatBps(399)).toBe('3.99%');
		expect(formatBps(-5)).toBe('-0.05%');
	});

	it('describes elapsed time', () => {
		const now = Date.parse('2026-03-01T12:00:00Z');
		expect(timeAgo('2026-03-01T11:59:40Z', now)).toBe('just now');
		expect(timeAgo('2026-03-01T11:30:00Z', now)).toBe('30m ago');
		expect(timeAgo('2026-03-01T07:00:00Z', now)).toBe('5h ago');
		expect(timeAgo('2026-02-27T12:00:00Z', now)).toBe('2d ago');
		expect(timeAgo('garbage', now)).toBe('');
	});
});

describe('command palette ranking', () => {
	const items: PaletteItem[] = [
		{ id: '1', group: 'Go to', label: 'Portfolio overview' },
		{ id: '2', group: 'Go to', label: 'Approval inbox', keywords: 'approve reject' },
		{ id: '3', group: 'Businesses', label: 'Kerala Spice Exports' },
		{ id: '4', group: 'Pending approvals', label: 'Pay Maersk ocean freight', hint: 'Kerala Spice Exports' },
		{ id: '5', group: 'Account', label: 'Sign out' }
	];

	it('scores exact > prefix > word start > substring > fuzzy > none', () => {
		const s = (q: string, t: string) => score(q, t);
		expect(s('sign out', 'Sign out')).toBeGreaterThan(s('sign', 'Sign out'));
		expect(s('sign', 'Sign out')).toBeGreaterThan(s('out', 'Sign out'));
		expect(s('out', 'Sign out')).toBeGreaterThan(s('gn', 'Sign out'));
		expect(s('gn', 'Sign out')).toBeGreaterThan(0);
		expect(s('zzz', 'Sign out')).toBe(0);
		expect(s('', 'anything')).toBeGreaterThan(0);
	});

	it('requires all characters in order', () => {
		expect(score('tuo', 'Sign out')).toBe(0);
	});

	it('ranks the best match first and drops non-matches', () => {
		expect(rank(items, 'kerala')[0].id).toBe('3');
		expect(rank(items, 'pay')[0].id).toBe('4');
		expect(rank(items, 'reject')[0].id).toBe('2'); // matched through keywords
		expect(rank(items, 'qqq')).toEqual([]);
	});

	it('keeps the given order for an empty query and honours the limit', () => {
		expect(rank(items, '').map((i) => i.id)).toEqual(['1', '2', '3', '4', '5']);
		expect(rank(items, '', 2)).toHaveLength(2);
	});

	it('is case-insensitive', () => {
		expect(rank(items, 'SIGN')[0].id).toBe('5');
	});
});

describe('blueprint helpers', () => {
	const agents: Agent[] = [
		{ key: 'ceo', role_title: 'CEO', responsibilities: [], tools: [], monthly_budget_minor: 0 },
		{ key: 'sales', role_title: 'Sales', reports_to: 'ceo', responsibilities: [], tools: [], monthly_budget_minor: 0 },
		{ key: 'support', role_title: 'Support', reports_to: 'sales', responsibilities: [], tools: [], monthly_budget_minor: 0 }
	];

	it('builds the org tree', () => {
		const t = orgTree(agents);
		expect(t).toHaveLength(1);
		expect(t[0].agent.key).toBe('ceo');
		expect(t[0].reports[0].agent.key).toBe('sales');
		expect(t[0].reports[0].reports[0].agent.key).toBe('support');
	});

	it('cannot loop on cycles, self-reports or dangling managers', () => {
		const cyc: Agent[] = [
			{ ...agents[0], key: 'a', reports_to: 'b' },
			{ ...agents[0], key: 'b', reports_to: 'a' },
			{ ...agents[0], key: 'c', reports_to: 'c' },
			{ ...agents[0], key: 'd', reports_to: 'ghost' }
		];
		expect(() => orgTree(cyc)).not.toThrow();
		const flat = JSON.stringify(orgTree(cyc), (_, v) => v).length;
		expect(flat).toBeGreaterThan(0);
	});

	it('groups accounts under their parent', () => {
		const g = accountGroups([
			{ code: '1000', name: 'Assets', type: 'ASSET', postable: false },
			{ code: '1010', name: 'Bank', type: 'ASSET', parent_code: '1000', postable: true },
			{ code: '3000', name: 'Equity', type: 'EQUITY', postable: true }
		]);
		expect(g.map((x) => x.account.code)).toEqual(['1000', '3000']);
		expect(g[0].children.map((c) => c.code)).toEqual(['1010']);
	});

	it('parses drafts defensively', () => {
		expect(parseBlueprint(undefined)).toBeNull();
		expect(parseBlueprint('')).toBeNull();
		expect(parseBlueprint('not json')).toBeNull();
		expect(parseBlueprint('{"x":1}')).toBeNull();
		expect(parseBlueprint(new TextEncoder().encode('{"business":{"name":"A"}}'))?.business.name).toBe('A');
	});
});

describe('approval payloads', () => {
	const enc = (v: unknown) => new TextEncoder().encode(JSON.stringify(v));

	it('presents a quote from engine numbers and surfaces its flags', () => {
		const v = parsePayload(
			enc({
				customer: 'Berlin GmbH', valid_days: 14,
				result: {
					currency: 'USD', price_minor: 1112196, cost_minor: 912000, margin_bps: 1800, margin_floor_bps: 1200, below_floor: false,
					components: [{ name: 'goods', amount_minor: 800000 }], approval_reasons: ['price meets the threshold']
				}
			}),
			'QUOTE', 'USD'
		);
		expect(v.kind).toBe('quote');
		expect(v.facts.find((f) => f.label === 'Quoted price')?.money).toEqual({ minor: 1112196n, currency: 'USD' });
		expect(v.facts.find((f) => f.label === 'Margin')).toMatchObject({ value: '18.00%', tone: 'good' });
		expect(v.components).toEqual([{ name: 'goods', minor: 800000n }]);
		expect(v.flags).toEqual(['price meets the threshold']);
	});

	it('marks a below-floor quote as bad', () => {
		const v = parsePayload(enc({ customer: 'x', result: { currency: 'USD', price_minor: 1, cost_minor: 2, margin_bps: -100, margin_floor_bps: 1200, below_floor: true } }), 'QUOTE', 'USD');
		expect(v.facts.find((f) => f.label === 'Margin')?.tone).toBe('bad');
	});

	it('presents an invoice with lines and totals', () => {
		const v = parsePayload(
			enc({
				counterparty: 'Berlin GmbH', currency: 'INR', tax_code: 'GST5', due_in_days: 30,
				totals: { lines: [{ description: 'Pepper', quantity: 1000, unit_price_minor: 1112, total_minor: 1112000 }], subtotal_minor: 1112000, tax_minor: 55600, total_minor: 1167600, tax_rate_bps: 500 }
			}),
			'INVOICE', 'INR'
		);
		expect(v.kind).toBe('document');
		expect(v.lines?.[0]).toEqual({ description: 'Pepper', quantity: 1000, unitMinor: 1112n, totalMinor: 1112000n });
		expect(v.totals).toMatchObject({ subtotal: 1112000n, tax: 55600n, total: 1167600n, currency: 'INR' });
	});

	it('presents a blocker as text', () => {
		const v = parsePayload(enc({ type: 'blocker', details: 'No supplier quote' }), 'OTHER', '');
		expect(v.kind).toBe('blocker');
		expect(v.text).toBe('No supplier quote');
	});

	it('falls back to labelled facts for anything else, formatting *_minor as money', () => {
		const v = parsePayload(enc({ payee: 'Maersk', amount_minor: 12000000 }), 'PAYMENT', 'INR');
		expect(v.kind).toBe('generic');
		expect(v.facts.find((f) => f.label === 'payee')?.value).toBe('Maersk');
		expect(v.facts.find((f) => f.label === 'amount')?.money).toEqual({ minor: 12000000n, currency: 'INR' });
	});

	it('survives malformed or empty payloads', () => {
		expect(parsePayload(undefined, 'OTHER', '').kind).toBe('generic');
		expect(parsePayload('not json', 'OTHER', '').kind).toBe('generic');
		expect(parsePayload(enc([1, 2]), 'OTHER', '').kind).toBe('generic');
	});

	it('asks for extra care on payments, high risk and flagged requests', () => {
		expect(needsExtraCare('PAYMENT', 'LOW', [])).toBe(true);
		expect(needsExtraCare('QUOTE', 'HIGH', [])).toBe(true);
		expect(needsExtraCare('QUOTE', 'LOW', ['below floor'])).toBe(true);
		expect(needsExtraCare('INVOICE', 'MEDIUM', [])).toBe(false);
		expect(RISK_ORDER.HIGH).toBeLessThan(RISK_ORDER.MEDIUM);
	});
});

describe('meeting approvals show exactly who would be emailed', () => {
	const view = (payload: object) => parsePayload(JSON.stringify(payload), 'MEETING', '');
	it('lists every attendee of an invitation and flags it', () => {
		const v = view({ action: 'schedule', meeting: { provider: 'google', title: 'Buyer call', start: '2026-03-10T09:30:00+05:30', duration_minutes: 30, timezone: 'Asia/Kolkata', attendees: ['ada@acme.com', 'bob@example.org'], notify: true } });
		const byLabel = Object.fromEntries(v.facts.map((f) => [f.label, f]));
		expect(byLabel['Will be emailed an invitation'].value).toBe('ada@acme.com, bob@example.org');
		expect(byLabel['Will be emailed an invitation'].tone).toBe('warn');
		expect(byLabel['Title'].value).toBe('Buyer call');
		expect(byLabel['Where'].value).toContain('Meet');
	});
	it('says plainly when a cancellation would notify people', () => {
		const v = view({ action: 'cancel', title: 'Buyer call', cancel: { provider: 'google', meeting_id: 'abc', notify: true } });
		expect(v.facts.find((f) => f.label === 'Attendees are told')?.value).toMatch(/emailed a cancellation/);
	});
	it('falls back to the generic listing for a shape it does not know', () => {
		const v = view({ action: 'mystery', note: 'x' });
		expect(v.kind).toBe('generic');
		expect(v.facts.map((f) => f.label)).toContain('action');
	});
});
