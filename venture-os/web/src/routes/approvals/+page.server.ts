import { error } from '@sveltejs/kit';
import { api, toFailure } from '$lib/server/api';
import { approvalView } from '$lib/server/map';
import type { PageServerLoad } from './$types';

const STATUSES = ['PENDING_APPROVAL', 'APPROVED', 'EXECUTED', 'REJECTED', 'FAILED'] as const;

export const load: PageServerLoad = async ({ locals, url, parent }) => {
	const { current } = await parent();
	const status = STATUSES.find((s) => s === url.searchParams.get('status')) ?? 'PENDING_APPROVAL';
	// ?b= overrides the saved switcher choice; "all" means every business.
	const requested = url.searchParams.get('b') ?? current;
	const tenantId = requested === 'all' ? '' : requested;
	try {
		const res = await api(locals.accessToken!).approval.listApprovals({ tenantId, status, limit: 100 });
		return { approvals: res.approvals.map(approvalView), status, scope: requested === '' ? 'all' : requested, statuses: [...STATUSES] };
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};
