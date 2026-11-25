import { error, fail } from '@sveltejs/kit';
import { parsePayload } from '$lib/approvals';
import { api, toFailure } from '$lib/server/api';
import { approvalView } from '$lib/server/map';
import type { Actions, PageServerLoad } from './$types';

export const load: PageServerLoad = async ({ locals, params }) => {
	try {
		const res = await api(locals.accessToken!).approval.getApproval({ tenantId: params.tenant, approvalId: params.id });
		if (!res.approval) error(404, 'Approval not found');
		const view = approvalView(res.approval);
		return {
			approval: view,
			payload: parsePayload(res.payloadJson, view.kind, view.currency),
			payloadHash: res.payloadHash,
			decidedBy: res.decidedBy,
			decisionNote: res.decisionNote,
			result: res.executionResultJson.length ? new TextDecoder().decode(res.executionResultJson) : ''
		};
	} catch (e) {
		if (e && typeof e === 'object' && 'status' in e) throw e; // an HttpError from error() above
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	decide: async ({ locals, params, request }) => {
		const form = await request.formData();
		const approve = form.get('decision') === 'approve';
		const note = String(form.get('note') ?? '').slice(0, 2000);
		const payloadHash = String(form.get('payloadHash') ?? '');
		try {
			const res = await api(locals.accessToken!).approval.decideApproval({
				tenantId: params.tenant,
				approvalId: params.id,
				approve,
				note,
				payloadHash
			});
			return {
				ok: true as const,
				approved: approve,
				status: res.status,
				executed: res.executed,
				executionError: res.executionError,
				result: res.executionResultJson.length ? new TextDecoder().decode(res.executionResultJson) : ''
			};
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	}
};
