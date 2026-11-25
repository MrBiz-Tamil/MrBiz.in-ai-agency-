import { fail } from '@sveltejs/kit';
import { decimalToMinor, fieldsFromForm, type Company, type Deal, type Page, type Pipeline } from '$lib/crm';
import { minorDigits } from '$lib/format';
import { toFailure } from '$lib/server/api';
import { act, crmCall, moveDeal, newKey, readAll } from '$lib/server/crm';
import { error } from '@sveltejs/kit';
import type { Actions, PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params, parent, url }) => {
	const { pipelines, unavailable } = await parent();
	if (unavailable || pipelines.length === 0) {
		return { pipeline: null, deals: [], companies: {} as Record<string, string>, truncated: false, companiesTruncated: false, q: '', newFor: '', keys: { deal: '' } };
	}
	const pipeline = pipelines.find((p) => p.key === url.searchParams.get('p')) ?? pipelines[0];
	const q = (url.searchParams.get('q') ?? '').trim().slice(0, 100);
	const token = locals.accessToken!;
	try {
		const [deals, companies] = await Promise.all([
			readAll<Deal>(async (cursor) =>
				(await crmCall<{ deals: Page<Deal> }>(token, params.tenant, 'crm_find_deals', { pipeline: pipeline.key, query: q || undefined, limit: 50, cursor })).deals
			),
			readAll<Company>(async (cursor) => (await crmCall<{ companies: Page<Company> }>(token, params.tenant, 'crm_find_companies', { limit: 50, cursor })).companies)
		]);
		return {
			pipeline,
			deals: deals.items,
			companies: Object.fromEntries(companies.items.map((c) => [c.id, c.name])) as Record<string, string>,
			// More than the board shows: say so rather than silently dropping cards.
			truncated: deals.truncated,
			companiesTruncated: companies.truncated,
			q,
			// Arrives from a company page ("New deal for ..."): open the form with that company chosen.
			newFor: url.searchParams.get('new') ?? '',
			keys: { deal: newKey() }
		};
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	move: async ({ locals, params, request }) => moveDeal(locals.accessToken!, params.tenant, await request.formData()),

	createDeal: async ({ locals, params, request }) => {
		const form = await request.formData();
		const token = locals.accessToken!;
		const text = (k: string) => String(form.get(k) ?? '').trim();
		let pipeline: Pipeline | undefined;
		try {
			// The pipeline's field definitions come from the server, never from the submitted form.
			pipeline = (await crmCall<{ pipelines: Pipeline[] }>(token, params.tenant, 'crm_pipelines')).pipelines.find((p) => p.key === text('pipeline'));
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
		if (!pipeline) return fail(400, { ok: false as const, message: 'Choose a pipeline.' });

		const args: Record<string, unknown> = {
			name: text('name'),
			company_id: text('company_id'),
			pipeline: pipeline.key,
			stage: text('stage'),
			fields: fieldsFromForm(form, pipeline.custom_fields),
			idempotency_key: text('key')
		};
		if (text('amount')) {
			const currency = text('currency').toUpperCase();
			const minor = decimalToMinor(text('amount'), minorDigits(currency || 'USD'));
			if (minor === null || !currency) return fail(400, { ok: false as const, message: 'Enter the amount like 48000 or 48,000.50, with a 3-letter currency such as USD.' });
			args.amount_minor = Number(minor); // the CRM caps amounts at 10^15, which a double holds exactly
			args.currency = currency;
		}
		if (text('expected_close')) args.expected_close = text('expected_close');
		return act(token, params.tenant, 'crm_create_deal', args);
	}
};
