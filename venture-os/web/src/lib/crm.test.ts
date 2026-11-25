import { describe, expect, it } from 'vitest';
import {
	actorLabel,
	columnTotals,
	companiesCsv,
	dealsCsv,
	csvCell,
	toCsv,
	decimalToMinor,
	fieldsFromForm,
	forPeople,
	groupByStage,
	inputType,
	isOverdue,
	type Activity,
	type Deal,
	type Pipeline
} from './crm';

const pipeline: Pipeline = {
	key: 'sales',
	name: 'Sales',
	stages: [
		{ key: 'lead', name: 'Lead', win_probability_bps: 1000 },
		{ key: 'won', name: 'Won', win_probability_bps: 10000 }
	],
	custom_fields: [
		{ key: 'incoterm', label: 'Incoterm', type: 'SELECT', required: true },
		{ key: 'volume', label: 'Volume', type: 'NUMBER', required: false },
		{ key: 'start', label: 'Start', type: 'DATE', required: false }
	]
};

const deal = (id: string, stage: string, amount?: [number, string]): Deal => ({
	id,
	name: id,
	company_id: 'c',
	pipeline: 'sales',
	stage,
	amount: amount ? { amount_minor: amount[0], currency: amount[1] } : undefined,
	stage_changed_at: '',
	created_at: ''
});

describe('decimalToMinor parses typed amounts exactly', () => {
	it('accepts plain, grouped and fractional amounts', () => {
		expect(decimalToMinor('48000', 2)).toBe(4800000n);
		expect(decimalToMinor('48,000.50', 2)).toBe(4800050n);
		expect(decimalToMinor('0.05', 2)).toBe(5n);
		expect(decimalToMinor('  12.5 ', 2)).toBe(1250n);
		expect(decimalToMinor('1,00,000', 2)).toBeNull(); // grouping must be thousands
		expect(decimalToMinor('500', 0)).toBe(500n);
		expect(decimalToMinor('1.250', 3)).toBe(1250n);
	});
	it('never rounds', () => {
		expect(decimalToMinor('1.005', 2)).toBeNull();
		expect(decimalToMinor('10.5', 0)).toBeNull();
	});
	it('rejects anything that is not an amount', () => {
		for (const bad of ['', ' ', 'abc', '-5', '+5', '1e3', '1.2.3', '$5', '5,', ',5', '1,23', '.5', '5.']) {
			expect(decimalToMinor(bad, 2), bad).toBeNull();
		}
	});
	it('is exact at the CRM ceiling and refuses anything above it', () => {
		expect(decimalToMinor('999999999999999', 0)).toBe(999999999999999n);
		expect(decimalToMinor('9007199254740993', 0)).toBeNull(); // above the ceiling (and above 2^53)
		expect(decimalToMinor('10000000000000.00', 2)).toBe(1_000_000_000_000_000n);
		expect(decimalToMinor('10000000000000.01', 2)).toBeNull();
	});
});

describe('board grouping and totals', () => {
	it('puts deals in stage order and keeps empty stages', () => {
		const cols = groupByStage(pipeline, [deal('a', 'won'), deal('b', 'lead'), deal('c', 'lead')]);
		expect(cols.map((c) => [c.stage.key, c.deals.map((d) => d.id)])).toEqual([
			['lead', ['b', 'c']],
			['won', ['a']]
		]);
		expect(groupByStage(pipeline, []).every((c) => c.deals.length === 0)).toBe(true);
	});
	it('never hides a deal whose stage no longer exists', () => {
		const cols = groupByStage(pipeline, [deal('x', 'retired_stage')]);
		expect(cols).toHaveLength(3);
		expect(cols[2]).toMatchObject({ orphan: true, deals: [{ id: 'x' }] });
	});
	it('sums per currency and never across currencies', () => {
		const t = columnTotals([deal('a', 'lead', [100, 'USD']), deal('b', 'lead', [250, 'USD']), deal('c', 'lead', [900, 'EUR']), deal('d', 'lead')]);
		expect(t).toEqual([
			{ currency: 'EUR', minor: 900n },
			{ currency: 'USD', minor: 350n }
		]);
		expect(columnTotals([])).toEqual([]);
	});
});

describe('people and agents are labelled honestly', () => {
	it('labels actors', () => {
		expect(actorLabel('user:sub-me', 'sub-me')).toBe('You');
		expect(actorLabel('user:sub-other', 'sub-me')).toBe('A teammate');
		expect(actorLabel('agent:sales', 'sub-me')).toBe('Agent sales');
		expect(actorLabel('', 'sub-me')).toBe('Unknown');
		expect(actorLabel(undefined, 'sub-me')).toBe('Unknown');
		expect(actorLabel('seed', 'sub-me')).toBe('seed');
	});
});

describe('tasks', () => {
	const task = (over: Partial<Activity>): Activity => ({ id: 't', kind: 'TASK', subject: 's', created_at: '', ...over });
	it('is overdue only when open, due and past', () => {
		const now = Date.parse('2026-03-10T00:00:00Z');
		expect(isOverdue(task({ due_at: '2026-03-09T00:00:00Z' }), now)).toBe(true);
		expect(isOverdue(task({ due_at: '2026-03-11T00:00:00Z' }), now)).toBe(false);
		expect(isOverdue(task({ due_at: '2026-03-09T00:00:00Z', completed_at: '2026-03-09T01:00:00Z' }), now)).toBe(false);
		expect(isOverdue(task({}), now)).toBe(false);
		expect(isOverdue(task({ kind: 'NOTE', due_at: '2026-03-09T00:00:00Z' }), now)).toBe(false);
	});
});

describe('custom fields', () => {
	const form = (o: Record<string, string>) => {
		const f = new FormData();
		for (const [k, v] of Object.entries(o)) f.set(k, v);
		return f;
	};
	it('maps field types to inputs', () => {
		expect(pipeline.custom_fields.map(inputType)).toEqual(['text', 'number', 'date']);
	});
	it('sets filled values and omits untouched empties on create', () => {
		expect(fieldsFromForm(form({ f_incoterm: ' FOB ', f_volume: '' }), pipeline.custom_fields)).toEqual({ incoterm: 'FOB' });
	});
	it('removes a value only when the record had one', () => {
		const f = form({ f_incoterm: 'CIF', f_volume: '', f_start: '' });
		expect(fieldsFromForm(f, pipeline.custom_fields, { incoterm: 'FOB', volume: '5' })).toEqual({ incoterm: 'CIF', volume: '' });
	});
	it('ignores inputs that are not the pipeline\'s fields', () => {
		expect(fieldsFromForm(form({ f_evil: 'x', f_incoterm: 'FOB' }), pipeline.custom_fields)).toEqual({ incoterm: 'FOB' });
	});
});

describe('messages for people', () => {
	it('rewords the agent-facing stale-version refusal and leaves everything else alone', () => {
		expect(forPeople('the record changed since you read it; fetch it again and retry with its new version')).toMatch(/Reload the page/);
		expect(forPeople('pipeline "sales" has no stage "x"')).toBe('pipeline "sales" has no stage "x"');
	});
});

describe('CSV export', () => {
	it('quotes cells that need it', () => {
		expect(csvCell('plain')).toBe('plain');
		expect(csvCell('a,b')).toBe('"a,b"');
		expect(csvCell('say "hi"')).toBe('"say ""hi"""');
		expect(csvCell('line\nbreak')).toBe('"line\nbreak"');
		expect(csvCell(null)).toBe('');
		expect(csvCell(undefined)).toBe('');
		expect(csvCell(1250)).toBe('1250');
		expect(csvCell(12n)).toBe('12');
	});
	it('defuses spreadsheet formulas typed by customers', () => {
		for (const evil of ['=HYPERLINK("http://evil","x")', '+1+1', '-2+3', '@SUM(A1)', '\tcmd', '\rcmd']) {
			expect(csvCell(evil).replace(/^"/, '').startsWith("'"), evil).toBe(true);
		}
		expect(csvCell('=1+1')).toBe("'=1+1");
		expect(csvCell(-5)).toBe('-5'); // a real number is not text and is left alone
		expect(csvCell('Acme = best')).toBe('Acme = best'); // only a leading character matters
	});
	it('builds CRLF-terminated rows with the header first', () => {
		expect(toCsv(['a', 'b'], [['1', 'x,y'], [2, null]])).toBe('a,b\r\n1,"x,y"\r\n2,\r\n');
	});
});

describe('export files', () => {
	it('writes one deal per row with exact values, stage names and the pipeline\'s fields', () => {
		const csv = dealsCsv(
			pipeline,
			[{ ...deal('d1', 'won', [4800050, 'USD']), name: 'Pepper, 2 x 20ft', fields: { incoterm: 'FOB' }, expected_close: '2026-04-30' }, deal('d2', 'lead')],
			{ c: 'Acme "Spices"' }
		);
		const lines = csv.trimEnd().split('\r\n');
		expect(lines[0]).toBe('Deal,Company,Pipeline,Stage,Value,Currency,Expected close,Created,Incoterm,Volume,Start,Deal ID,Company ID');
		expect(lines[1]).toBe('"Pepper, 2 x 20ft","Acme ""Spices""",Sales,Won,48000.50,USD,2026-04-30,,FOB,,,d1,c');
		expect(lines[2]).toBe('d2,"Acme ""Spices""",Sales,Lead,,,,,,,,d2,c');
	});
	it('does not run a customer\'s formula', () => {
		const csv = companiesCsv([{ id: 'x', name: '=cmd|\' /C calc\'!A0', created_at: 't' }]);
		expect(csv.split('\r\n')[1].startsWith("'=cmd")).toBe(true);
	});
});
