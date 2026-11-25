import { error } from '@sveltejs/kit';
import { companiesCsv, dealsCsv, type Company, type Deal, type Page, type Pipeline } from '$lib/crm';
import { toFailure } from '$lib/server/api';
import { crmCall, readAll } from '$lib/server/crm';
import type { RequestHandler } from './$types';

// CSV export of what the signed-in person may already read (the control plane applies the role check). Capped so one
// request cannot become hundreds of calls; over the cap it FAILS and says how to narrow, rather than silently
// handing over a file that looks complete but is not.
const MAX_PAGES = 100; // 5,000 rows

const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'export';

export const GET: RequestHandler = async ({ locals, params, url }) => {
	const token = locals.accessToken!;
	const q = (url.searchParams.get('q') ?? '').trim().slice(0, 100) || undefined;
	const type = url.searchParams.get('type');
	try {
		const companies = () =>
			readAll<Company>(async (cursor) => (await crmCall<{ companies: Page<Company> }>(token, params.tenant, 'crm_find_companies', { query: type === 'companies' ? q : undefined, limit: 50, cursor })).companies, MAX_PAGES);
		const tooMany = (what: string) => error(413, `There are more than ${MAX_PAGES * 50} ${what} to export in one file. Search to narrow it down first.`);
		const day = new Date().toISOString().slice(0, 10);
		let body: string;
		let name: string;
		if (type === 'companies') {
			const all = await companies();
			if (all.truncated) tooMany('companies');
			body = companiesCsv(all.items);
			name = `companies-${day}.csv`;
		} else if (type === 'deals') {
			const { pipelines } = await crmCall<{ pipelines: Pipeline[] }>(token, params.tenant, 'crm_pipelines');
			const pipeline = pipelines.find((p) => p.key === url.searchParams.get('p')) ?? pipelines[0];
			if (!pipeline) error(404, 'This business has no pipelines.');
			const [deals, cos] = await Promise.all([
				readAll<Deal>(async (cursor) => (await crmCall<{ deals: Page<Deal> }>(token, params.tenant, 'crm_find_deals', { pipeline: pipeline.key, query: q, limit: 50, cursor })).deals, MAX_PAGES),
				companies()
			]);
			if (deals.truncated) tooMany('deals');
			body = dealsCsv(pipeline, deals.items, Object.fromEntries(cos.items.map((c) => [c.id, c.name])));
			name = `deals-${slug(pipeline.name)}-${day}.csv`;
		} else {
			error(400, 'Choose what to export: type=deals or type=companies.');
		}
		return new Response('\uFEFF' + body, {
			headers: {
				'content-type': 'text/csv; charset=utf-8',
				'content-disposition': `attachment; filename="${name}"`,
				'cache-control': 'private, no-store'
			}
		});
	} catch (e) {
		if (e && typeof e === 'object' && 'status' in e) throw e; // an HttpError raised above
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};
