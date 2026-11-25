import { error } from '@sveltejs/kit';
import { ConnectError, Code } from '@connectrpc/connect';
import { api, toFailure } from '$lib/server/api';
import { approvalView, stepView } from '$lib/server/map';
import type { PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params, parent }) => {
	const { businesses } = await parent();
	const business = businesses.find((b) => b.id === params.tenant);
	if (!business) error(404, 'We could not find that business.');
	const a = api(locals.accessToken!);
	try {
		const [run, pending] = await Promise.all([
			a.provisioner.getStatus({ tenantId: params.tenant }).catch((e) => {
				if (e instanceof ConnectError && e.code === Code.NotFound) return null; // never provisioned
				throw e;
			}),
			business.role === 'viewer' ? null : a.approval.listApprovals({ tenantId: params.tenant, limit: 5 })
		]);
		return {
			business,
			run: run ? { status: run.status, error: run.error, steps: run.steps.map(stepView) } : null,
			pending: pending ? pending.approvals.map(approvalView) : []
		};
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};
