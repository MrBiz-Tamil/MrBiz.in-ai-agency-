import { error, redirect } from '@sveltejs/kit';
import { config } from '$lib/server/config';
import { LOGIN_COOKIE, completeLogin, type LoginState } from '$lib/server/oidc';
import { CHUNK_SIZE, SESSION_COOKIE, chunk, open, safeReturnTo, seal } from '$lib/server/session';
import type { RequestHandler } from './$types';

export const GET: RequestHandler = async ({ url, cookies }) => {
	const cfg = config();
	const login = open<LoginState>(cfg.key, cookies.get(LOGIN_COOKIE));
	cookies.delete(LOGIN_COOKIE, { path: '/auth' });
	if (!login) error(400, 'Your sign-in attempt expired. Please try again.');
	// Rebuild the callback URL on our own origin: behind a proxy event.url may carry the internal host.
	const callback = new URL(url.pathname + url.search, cfg.origin);
	let session;
	try {
		session = await completeLogin(callback, login);
	} catch {
		error(400, 'Sign-in could not be completed. Please try again.');
	}
	for (const c of chunk(SESSION_COOKIE, seal(cfg.key, session), CHUNK_SIZE)) {
		cookies.set(c.name, c.value, { path: '/', httpOnly: true, sameSite: 'lax', secure: cfg.secureCookies });
	}
	redirect(303, safeReturnTo(login.returnTo));
};
