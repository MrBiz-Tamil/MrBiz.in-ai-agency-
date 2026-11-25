import { minorDigits, minorToDecimal } from './format';

// CRM view types and the small pure helpers the CRM screens share. Shapes mirror internal/crmops results.
// Nothing here touches the network, so all of it is unit-tested.

export interface Stage {
	key: string;
	name: string;
	win_probability_bps: number;
}
export interface FieldDef {
	key: string;
	label: string;
	type: string;
	required: boolean;
}
export interface Pipeline {
	key: string;
	name: string;
	stages: Stage[];
	custom_fields: FieldDef[];
}
export interface Money {
	amount_minor: number;
	currency: string;
}
export interface Company {
	id: string;
	name: string;
	domain?: string;
	country?: string;
	industry?: string;
	website?: string;
	phone?: string;
	fields?: Record<string, string>;
	version?: number;
	created_at: string;
}
export interface Contact {
	id: string;
	company_id?: string;
	first_name?: string;
	last_name?: string;
	email?: string;
	phone?: string;
	title?: string;
	version?: number;
	created_at: string;
}
export interface Deal {
	id: string;
	name: string;
	company_id: string;
	contact_id?: string;
	pipeline: string;
	stage: string;
	amount?: Money;
	expected_close?: string;
	fields?: Record<string, string>;
	stage_changed_at: string;
	version?: number;
	created_at: string;
}
export interface Activity {
	id: string;
	kind: string;
	subject: string;
	body?: string;
	body_truncated?: boolean;
	due_at?: string;
	completed_at?: string;
	company_id?: string;
	contact_id?: string;
	deal_id?: string;
	created_by?: string;
	created_at: string;
}
export interface StageChange {
	from?: string;
	to: string;
	changed_by?: string;
	changed_at: string;
}
export interface Page<T> {
	items: T[];
	next_cursor?: string;
}

export const ACTIVITY_KINDS = ['NOTE', 'CALL', 'EMAIL', 'MEETING', 'TASK'] as const;

/**
 * Parses what a person typed as an amount ("48000", "48,000.50") into exact minor units, or null if it is not a
 * valid amount for a currency with `digits` minor digits. It never rounds: "1.005" for USD is rejected, not
 * silently turned into 1.00 or 1.01. No floating point is involved.
 */
export function decimalToMinor(text: string, digits: number): bigint | null {
	const t = text.trim();
	if (!/^(\d+|\d{1,3}(,\d{3})+)(\.\d+)?$/.test(t)) return null;
	const [whole, frac = ''] = t.replace(/,/g, '').split('.');
	if (frac.length > digits) return null;
	const minor = BigInt(whole + frac.padEnd(digits, '0'));
	return minor <= 1_000_000_000_000_000n ? minor : null; // the CRM's own ceiling
}

/** Deals grouped into the pipeline's stages, in order. A deal whose stage the pipeline no longer has is not hidden. */
export function groupByStage(p: Pipeline, deals: Deal[]): { stage: Stage; deals: Deal[]; orphan: boolean }[] {
	const cols = p.stages.map((stage) => ({ stage, deals: [] as Deal[], orphan: false }));
	const byKey = new Map(cols.map((c) => [c.stage.key, c]));
	const orphans: Deal[] = [];
	for (const d of deals) (byKey.get(d.stage)?.deals ?? orphans).push(d);
	if (orphans.length) cols.push({ stage: { key: '?', name: 'Unknown stage', win_probability_bps: 0 }, deals: orphans, orphan: true });
	return cols;
}

/** Sum of deal amounts per currency (amounts in different currencies are never added together). */
export function columnTotals(deals: Deal[]): { currency: string; minor: bigint }[] {
	const sums = new Map<string, bigint>();
	for (const d of deals) if (d.amount) sums.set(d.amount.currency, (sums.get(d.amount.currency) ?? 0n) + BigInt(d.amount.amount_minor));
	return [...sums].map(([currency, minor]) => ({ currency, minor })).sort((a, b) => a.currency.localeCompare(b.currency));
}

/** "You", "Agent sales" or "A teammate" for a changed_by / created_by value ("user:<sub>" or "agent:<key>"). */
export function actorLabel(by: string | undefined, mySub: string): string {
	if (!by) return 'Unknown';
	if (by === `user:${mySub}`) return 'You';
	if (by.startsWith('user:')) return 'A teammate';
	if (by.startsWith('agent:')) return `Agent ${by.slice(6)}`;
	return by;
}

/** A task is overdue when it has a due time in the past and is not done. */
export function isOverdue(a: Activity, now = Date.now()): boolean {
	return a.kind === 'TASK' && !a.completed_at && !!a.due_at && Date.parse(a.due_at) < now;
}

/** The HTML input type that fits a custom field. SELECT has no option list in the blueprint, so it is free text. */
export function inputType(f: FieldDef): 'number' | 'date' | 'text' {
	return f.type === 'NUMBER' ? 'number' : f.type === 'DATE' ? 'date' : 'text';
}

/**
 * Reads a pipeline's custom fields from a submitted form (inputs are named f_<key>). A filled value is set; an
 * emptied one is sent as "" only when the record had a value (which removes it); an untouched empty one is omitted.
 */
export function fieldsFromForm(form: FormData, defs: FieldDef[], current?: Record<string, string>): Record<string, string> {
	const out: Record<string, string> = {};
	for (const d of defs) {
		const v = String(form.get(`f_${d.key}`) ?? '').trim();
		if (v !== '') out[d.key] = v;
		else if (current?.[d.key]) out[d.key] = '';
	}
	return out;
}

/** The control plane words a stale-version refusal for agents; people need to know what to do next. */
export function forPeople(message: string): string {
	return /changed since you read it/i.test(message)
		? 'Someone else changed this while you were looking at it. Reload the page to see their change, then try again.'
		: message;
}

/**
 * One CSV cell. Quotes per RFC 4180, and defuses spreadsheet formula injection: a cell that starts with = + - @ (or a
 * tab or carriage return) would be run as a formula by Excel or Sheets, and CRM text is typed by customers, so it is
 * prefixed with an apostrophe, which spreadsheets display as plain text. Real numbers are passed as numbers and are
 * never touched.
 */
export function csvCell(v: string | number | bigint | null | undefined): string {
	if (v === null || v === undefined) return '';
	let t = String(v);
	if (typeof v === 'string' && /^[=+\-@\t\r]/.test(t)) t = `'${t}`;
	return /[",\r\n]/.test(t) ? `"${t.replace(/"/g, '""')}"` : t;
}

/** Rows to CSV text with CRLF line ends (what spreadsheets expect). */
export function toCsv(header: string[], rows: (string | number | bigint | null | undefined)[][]): string {
	return [header, ...rows].map((r) => r.map(csvCell).join(',')).join('\r\n') + '\r\n';
}

/** The deals of one pipeline as CSV: what a person would want in a spreadsheet, plus the ids to join on. */
export function dealsCsv(p: Pipeline, deals: Deal[], companyNames: Record<string, string>): string {
	const stageName = new Map(p.stages.map((s) => [s.key, s.name]));
	const header = ['Deal', 'Company', 'Pipeline', 'Stage', 'Value', 'Currency', 'Expected close', 'Created', ...p.custom_fields.map((f) => f.label), 'Deal ID', 'Company ID'];
	const rows = deals.map((d) => [
		d.name,
		companyNames[d.company_id] ?? '',
		p.name,
		stageName.get(d.stage) ?? d.stage,
		d.amount ? minorToDecimal(BigInt(d.amount.amount_minor), minorDigits(d.amount.currency)) : '',
		d.amount?.currency ?? '',
		d.expected_close ?? '',
		d.created_at,
		...p.custom_fields.map((f) => d.fields?.[f.key] ?? ''),
		d.id,
		d.company_id
	]);
	return toCsv(header, rows);
}

export function companiesCsv(companies: Company[]): string {
	return toCsv(
		['Name', 'Domain', 'Country', 'Industry', 'Website', 'Phone', 'Created', 'Company ID'],
		companies.map((c) => [c.name, c.domain, c.country, c.industry, c.website, c.phone, c.created_at, c.id])
	);
}
