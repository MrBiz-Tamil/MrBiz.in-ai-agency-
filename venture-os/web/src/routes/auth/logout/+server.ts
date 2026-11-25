import { redirect } from '@sveltejs/kit';
import { config } from '$lib/server/config';
import { logoutUrl } from '$lib/server/oidc';
import { SESSION_COOKIE } from '$lib/server/session';
import type { RequestHandler } from './$types';

// POST only: a GET must never sign anyone out (an <img src> on another site could otherwise do it).
export const POST: RequestHandler = async ({ cookies }) => {
	for (let i = 0; i < 8; i++) cookies.delete(`${SESSION_COOKIE}.${i}`, { path: '/' });
	if (config().authMode === 'oidc') {
		const u = await logoutUrl().catch(() => null);
		if (u) redirect(303, u.toString());
	}
	redirect(303, '/');
};
