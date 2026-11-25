import { error, fail, redirect } from '@sveltejs/kit';
import { decimalToMinor, fieldsFromForm, type Activity, type Deal, type Pipeline, type StageChange } from '$lib/crm';
import { minorDigits } from '$lib/format';
import { toFailure } from '$lib/server/api';
import { act, addActivity, completeTask, crmCall, moveDeal, newKey } from '$lib/server/crm';
import type { Actions, PageServerLoad } from './$types';

interface DealDetail {
	deal: Deal;
	company?: { id: string; name: string };
	stage_history?: StageChange[];
	activities: Activity[];
	activities_truncated: boolean;
}

export const load: PageServerLoad = async ({ locals, params, parent }) => {
	const { pipelines } = await parent();
	try {
		const r = await crmCall<DealDetail>(locals.accessToken!, params.tenant, 'crm_get_deal', { deal_id: params.id });
		return { ...r, pipeline: pipelines.find((p) => p.key === r.deal.pipeline) ?? null, keys: { activity: newKey() } };
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	move: async ({ locals, params, request }) => moveDeal(locals.accessToken!, params.tenant, await request.formData()),
	activity: async ({ locals, params, request }) => addActivity(locals.accessToken!, params.tenant, await request.formData()),
	complete: async ({ locals, params, request }) => completeTask(locals.accessToken!, params.tenant, await request.formData()),

	archive: async ({ locals, params, request }) => {
		const form = await request.formData();
		const v = Number(form.get('version'));
		const res = await act(locals.accessToken!, params.tenant, 'crm_archive_deal', { id: params.id, ...(Number.isSafeInteger(v) && v > 0 ? { expected_version: v } : {}) });
		if ('ok' in res && res.ok) redirect(303, `/b/${params.tenant}/crm`);
		return res;
	},

	update: async ({ locals, params, request }) => {
		const form = await request.formData();
		const token = locals.accessToken!;
		const text = (k: string) => String(form.get(k) ?? '').trim();
		try {
			// Current state and field definitions come from the server, never from the submitted form.
			const [{ deal }, { pipelines }] = await Promise.all([
				crmCall<DealDetail>(token, params.tenant, 'crm_get_deal', { deal_id: params.id }),
				crmCall<{ pipelines: Pipeline[] }>(token, params.tenant, 'crm_pipelines')
			]);
			const pipeline = pipelines.find((p) => p.key === deal.pipeline);
			if (!pipeline) return fail(400, { ok: false as const, message: 'This deal’s pipeline is no longer active, so it cannot be edited.' });

			const args: Record<string, unknown> = {
				deal_id: params.id,
				name: text('name'),
				fields: fieldsFromForm(form, pipeline.custom_fields, deal.fields)
			};
			const v = Number(form.get('version'));
			if (Number.isSafeInteger(v) && v > 0) args.expected_version = v;

			if (text('amount')) {
				const currency = text('currency').toUpperCase();
				const minor = decimalToMinor(text('amount'), minorDigits(currency || 'USD'));
				if (minor === null || !currency) return fail(400, { ok: false as const, message: 'Enter the amount like 48000 or 48,000.50, with a 3-letter currency such as USD.' });
				args.amount_minor = Number(minor);
				args.currency = currency;
			} else if (deal.amount) args.clear_amount = true;

			if (text('expected_close')) args.expected_close = text('expected_close');
			else if (deal.expected_close) args.clear_expected_close = true;

			return await act(token, params.tenant, 'crm_update_deal', args, () => ({ saved: true as const }));
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	}
};
