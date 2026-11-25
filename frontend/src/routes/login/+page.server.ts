import { dev } from '$app/environment';
import { fail, redirect } from '@sveltejs/kit';
import { backendFetch, SESSION_COOKIE } from '$lib/server/backend';
import type { Actions, PageServerLoad } from './$types';

// Only same-site relative paths may be used as the post-login destination (no open redirect).
function safeNext(raw: string | null): string {
	if (raw && raw.startsWith('/') && !raw.startsWith('//') && !raw.includes('\\')) return raw;
	return '/operations';
}

export const load: PageServerLoad = ({ locals, url }) => {
	if (locals.userSession) redirect(303, safeNext(url.searchParams.get('next')));
	return { next: safeNext(url.searchParams.get('next')) };
};

export const actions: Actions = {
	default: async ({ request, cookies, url }) => {
		const form = await request.formData();
		const email = String(form.get('email') ?? '').trim();
		const password = String(form.get('password') ?? '');
		const next = safeNext(String(form.get('next') ?? url.searchParams.get('next') ?? ''));

		if (!email || !password) return fail(400, { email, error: 'Enter your email and password.' });

		let res: Response;
		try {
			res = await backendFetch('/auth/login', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ email, password })
			});
		} catch {
			return fail(503, { email, error: 'The server is unreachable. Try again shortly.' });
		}
		if (res.status === 429) return fail(429, { email, error: 'Too many attempts. Wait a few minutes and try again.' });
		if (res.status === 401) return fail(401, { email, error: 'Invalid email or password.' });
		if (!res.ok) return fail(502, { email, error: 'Sign-in is unavailable right now.' });

		const { token, expiresAt } = (await res.json()) as { token: string; expiresAt: string };
		const maxAge = Math.max(60, Math.floor((new Date(expiresAt).getTime() - Date.now()) / 1000));
		cookies.set(SESSION_COOKIE, token, {
			path: '/',
			httpOnly: true, // never readable from page JavaScript
			secure: !dev,
			sameSite: 'strict',
			maxAge
		});
		redirect(303, next);
	}
};
