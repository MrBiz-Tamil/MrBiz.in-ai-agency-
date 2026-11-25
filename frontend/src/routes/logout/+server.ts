import { redirect } from '@sveltejs/kit';
import { backendFetch, forgetSession, SESSION_COOKIE } from '$lib/server/backend';
import type { RequestHandler } from './$types';

// POST only: a GET (link, image tag, prefetch) must never be able to log a user out.
export const POST: RequestHandler = async ({ cookies, locals }) => {
	const token = locals.sessionToken;
	if (token) {
		forgetSession(token);
		try {
			await backendFetch('/auth/logout', { method: 'POST' }, token, 5000);
		} catch {
			// The cookie is removed below regardless; the backend session expires on its own.
		}
	}
	cookies.delete(SESSION_COOKIE, { path: '/' });
	redirect(303, '/login');
};
