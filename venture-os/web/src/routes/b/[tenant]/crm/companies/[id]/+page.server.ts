import { error, redirect } from '@sveltejs/kit';
import type { Activity, Company, Contact, Deal } from '$lib/crm';
import { toFailure } from '$lib/server/api';
import { act, addActivity, completeTask, crmCall, newKey } from '$lib/server/crm';
import type { Actions, PageServerLoad } from './$types';

interface CompanyDetail {
	company: Company;
	contacts: Contact[];
	contacts_truncated: boolean;
	deals: Deal[];
	deals_truncated: boolean;
	open_tasks: Activity[];
	open_tasks_truncated: boolean;
}

export const load: PageServerLoad = async ({ locals, params }) => {
	try {
		const r = await crmCall<CompanyDetail>(locals.accessToken!, params.tenant, 'crm_get_company', { company_id: params.id });
		return { ...r, keys: { contact: newKey(), activity: newKey() } };
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	activity: async ({ locals, params, request }) => addActivity(locals.accessToken!, params.tenant, await request.formData()),
	complete: async ({ locals, params, request }) => completeTask(locals.accessToken!, params.tenant, await request.formData()),

	update: async ({ locals, params, request }) => {
		const form = await request.formData();
		const text = (k: string) => String(form.get(k) ?? '').trim();
		// Every detail is sent, so clearing a box clears that detail; the version makes a stale edit fail.
		const args: Record<string, unknown> = { company_id: params.id };
		for (const k of ['name', 'domain', 'country', 'industry', 'website', 'phone']) args[k] = text(k);
		const v = Number(form.get('version'));
		if (Number.isSafeInteger(v) && v > 0) args.expected_version = v;
		return act(locals.accessToken!, params.tenant, 'crm_update_company', args, () => ({ saved: true as const }));
	},

	archive: async ({ locals, params, request }) => {
		const form = await request.formData();
		const v = Number(form.get('version'));
		const res = await act(locals.accessToken!, params.tenant, 'crm_archive_company', { id: params.id, ...(Number.isSafeInteger(v) && v > 0 ? { expected_version: v } : {}) });
		if ('ok' in res && res.ok) redirect(303, `/b/${params.tenant}/crm/companies`);
		return res;
	},

	updateContact: async ({ locals, params, request }) => {
		const form = await request.formData();
		const text = (k: string) => String(form.get(k) ?? '').trim();
		const args: Record<string, unknown> = { contact_id: text('contact_id') };
		for (const k of ['first_name', 'last_name', 'email', 'phone', 'title']) args[k] = text(k);
		const v = Number(form.get('version'));
		if (Number.isSafeInteger(v) && v > 0) args.expected_version = v;
		return act(locals.accessToken!, params.tenant, 'crm_update_contact', args, () => ({ saved: true as const }));
	},

	archiveContact: async ({ locals, params, request }) => {
		const form = await request.formData();
		const v = Number(form.get('version'));
		return act(locals.accessToken!, params.tenant, 'crm_archive_contact', {
			id: String(form.get('contact_id') ?? ''),
			...(Number.isSafeInteger(v) && v > 0 ? { expected_version: v } : {})
		});
	},

	contact: async ({ locals, params, request }) => {
		const form = await request.formData();
		const text = (k: string) => String(form.get(k) ?? '').trim();
		const args: Record<string, string> = { company_id: params.id, idempotency_key: text('key') };
		for (const k of ['first_name', 'last_name', 'email', 'phone', 'title']) if (text(k)) args[k] = text(k);
		return act(locals.accessToken!, params.tenant, 'crm_create_contact', args);
	}
};
