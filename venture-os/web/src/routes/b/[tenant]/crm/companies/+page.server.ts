import { error, redirect } from '@sveltejs/kit';
import type { Company, Page } from '$lib/crm';
import { toFailure } from '$lib/server/api';
import { act, crmCall, newKey } from '$lib/server/crm';
import type { Actions, PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params, url }) => {
	const q = (url.searchParams.get('q') ?? '').trim().slice(0, 100);
	const cursor = url.searchParams.get('cursor') ?? undefined;
	try {
		const { companies } = await crmCall<{ companies: Page<Company> }>(locals.accessToken!, params.tenant, 'crm_find_companies', { query: q || undefined, limit: 50, cursor });
		return { companies: companies.items, next: companies.next_cursor ?? null, q, keys: { company: newKey() } };
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	create: async ({ locals, params, request }) => {
		const form = await request.formData();
		const text = (k: string) => String(form.get(k) ?? '').trim();
		const args: Record<string, string> = { name: text('name'), idempotency_key: text('key') };
		for (const k of ['domain', 'country', 'industry', 'phone', 'website']) if (text(k)) args[k] = text(k);
		const res = await act<{ company: Company }>(locals.accessToken!, params.tenant, 'crm_create_company', args, (r) => ({ id: r.company.id }));
		if (res && 'id' in res) redirect(303, `/b/${params.tenant}/crm/companies/${res.id}`);
		return res;
	}
};
