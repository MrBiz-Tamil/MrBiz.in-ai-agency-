import { error } from '@sveltejs/kit';
import type { Activity, Page } from '$lib/crm';
import { toFailure } from '$lib/server/api';
import { completeTask, crmCall } from '$lib/server/crm';
import type { Actions, PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params, url }) => {
	const cursor = url.searchParams.get('cursor') ?? undefined;
	try {
		const { tasks } = await crmCall<{ tasks: Page<Activity> }>(locals.accessToken!, params.tenant, 'crm_open_tasks', { limit: 50, cursor });
		return { tasks: tasks.items, next: tasks.next_cursor ?? null };
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	complete: async ({ locals, params, request }) => completeTask(locals.accessToken!, params.tenant, await request.formData())
};
