import { error } from '@sveltejs/kit';
import type { Pipeline } from '$lib/crm';
import { toFailure } from '$lib/server/api';
import { crmCall, isNoCrm } from '$lib/server/crm';
import type { LayoutServerLoad } from './$types';

// Everything under /b/<business>/crm needs the business, the caller's role and the pipelines, so they load once here.
export const load: LayoutServerLoad = async ({ locals, params, parent }) => {
	const { businesses } = await parent();
	const business = businesses.find((b) => b.id === params.tenant);
	if (!business) error(404, 'We could not find that business.');
	const base = { business, me: locals.user!.sub, canWrite: business.role !== 'viewer' };
	try {
		const { pipelines } = await crmCall<{ pipelines: Pipeline[] }>(locals.accessToken!, params.tenant, 'crm_pipelines');
		return { ...base, pipelines, unavailable: null as string | null };
	} catch (e) {
		if (isNoCrm(e)) return { ...base, canWrite: false, pipelines: [] as Pipeline[], unavailable: 'This business has no CRM connected, so there is nothing to show here yet.' };
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};
