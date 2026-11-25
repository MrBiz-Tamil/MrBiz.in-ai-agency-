import { redirect } from '@sveltejs/kit';
import { config } from '$lib/server/config';
import { LOGIN_COOKIE, beginLogin } from '$lib/server/oidc';
import { safeReturnTo, seal } from '$lib/server/session';
import type { RequestHandler } from './$types';

export const GET: RequestHandler = async ({ url, cookies }) => {
	const cfg = config();
	if (cfg.authMode === 'dev') redirect(303, '/');
	const { url: providerUrl, state } = await beginLogin(safeReturnTo(url.searchParams.get('returnTo')));
	cookies.set(LOGIN_COOKIE, seal(cfg.key, state), {
		path: '/auth', httpOnly: true, sameSite: 'lax', secure: cfg.secureCookies, maxAge: 600
	});
	redirect(303, providerUrl.toString());
};
