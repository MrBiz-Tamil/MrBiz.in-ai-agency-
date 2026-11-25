import { error } from '@sveltejs/kit';
import { BIZ_COOKIE } from '$lib/constants';
import { api, toFailure } from '$lib/server/api';
import { config } from '$lib/server/config';
import { approvalView, businessView } from '$lib/server/map';
import type { LayoutServerLoad } from './$types';

export const load: LayoutServerLoad = async ({ locals, cookies, depends, url }) => {
	depends('app:shell');
	if (!locals.user || !locals.accessToken) {
		return { user: null, businesses: [], inbox: [], current: 'all', simulated: [] as string[], devAuth: false };
	}
	const a = api(locals.accessToken);
	try {
		const [portfolio, inbox] = await Promise.all([
			a.portfolio.getPortfolio({}),
			a.approval.listApprovals({ limit: 30 })
		]);
		const businesses = portfolio.businesses.map(businessView);
		// The business filter is a UI preference; it is validated against the user's own businesses on every load.
		const saved = cookies.get(BIZ_COOKIE) ?? 'all';
		const current = businesses.some((b) => b.id === saved) ? saved : 'all';
		return {
			user: locals.user,
			businesses,
			inbox: inbox.approvals.map(approvalView),
			current,
			simulated: config().simulated,
			devAuth: config().authMode === 'dev'
		};
	} catch (e) {
		const f = toFailure(e);
		// The page cannot render without the control plane; say so plainly rather than showing an empty portfolio.
		error(f.status === 401 ? 401 : 503, { message: f.status === 503 || f.status >= 500 ? 'The control plane is not reachable right now.' : f.message, code: f.code });
	}
};
