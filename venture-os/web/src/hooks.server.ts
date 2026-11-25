import type { Handle } from '@sveltejs/kit';
import { building } from '$app/environment';
import { env } from '$env/dynamic/private';
import { config } from '$lib/server/config';
import { refresh } from '$lib/server/oidc';
import {
	CHUNK_SIZE,
	SESSION_COOKIE,
	chunk,
	join,
	needsRefresh,
	open,
	seal,
	withinLifetime,
	type Session
} from '$lib/server/session';

const PUBLIC_PREFIXES = ['/auth/', '/healthz'];
// Development only (AUTH_MODE=dev): DEV_USER picks which seeded person you are, so each role can be tried in a browser.
const dev = (who: string): Session => ({ sub: `sub-dev-${who}`, name: `Dev ${who[0].toUpperCase()}${who.slice(1)}`, email: `${who}@dev.local`, at: `dev-${who}`, exp: 4102444800, iat: 0 });
const DEV_USERS: Record<string, Session> = { founder: dev('founder'), approver: dev('approver'), viewer: dev('viewer') };
function devSession(): Session {
	const who = (env.DEV_USER ?? 'founder').trim().toLowerCase();
	const s = DEV_USERS[who];
	if (!s) throw new Error(`DEV_USER must be one of ${Object.keys(DEV_USERS).join(', ')}, got "${who}"`);
	return s;
}

function cookieOpts(secure: boolean, maxAge?: number) {
	return { path: '/', httpOnly: true, sameSite: 'lax' as const, secure, ...(maxAge ? { maxAge } : {}) };
}

/** Security headers for every response we produce, including the early refusals and redirects below. */
function harden(response: Response, secureCookies: boolean): Response {
	response.headers.set('X-Content-Type-Options', 'nosniff');
	response.headers.set('Referrer-Policy', 'same-origin');
	response.headers.set('X-Frame-Options', 'DENY');
	response.headers.set('Cache-Control', response.headers.get('Cache-Control') ?? 'private, no-store');
	if (secureCookies) response.headers.set('Strict-Transport-Security', 'max-age=31536000; includeSubDomains');
	return response;
}

export const handle: Handle = async ({ event, resolve }) => {
	if (building) return resolve(event);
	const cfg = config();
	const { pathname } = event.url;

	// Defence in depth against cross-site writes (SameSite=Lax already withholds the cookie): an explicit
	// Origin on any state-changing request must be ours.
	if (!['GET', 'HEAD', 'OPTIONS'].includes(event.request.method)) {
		const origin = event.request.headers.get('origin');
		if (origin && origin !== cfg.origin && origin !== event.url.origin) {
			return harden(new Response('Cross-origin request refused', { status: 403 }), cfg.secureCookies);
		}
	}

	event.locals.user = null;
	event.locals.accessToken = null;

	if (cfg.authMode === 'dev') {
		const ds = devSession();
		event.locals.user = { sub: ds.sub, name: ds.name, email: ds.email };
		event.locals.accessToken = ds.at;
	} else {
		const sealed = join(SESSION_COOKIE, (n) => event.cookies.get(n));
		let session = open<Session>(cfg.key, sealed);
		if (session && !withinLifetime(session)) session = null;
		if (session && needsRefresh(session)) {
			const renewed = await refresh(session);
			session = renewed && withinLifetime(renewed) ? renewed : null;
			if (session) {
				for (const c of chunk(SESSION_COOKIE, seal(cfg.key, session), CHUNK_SIZE)) {
					event.cookies.set(c.name, c.value, cookieOpts(cfg.secureCookies));
				}
			}
		}
		if (session) {
			event.locals.user = { sub: session.sub, name: session.name, email: session.email };
			event.locals.accessToken = session.at;
		} else if (sealed) {
			for (let i = 0; i < 8; i++) event.cookies.delete(`${SESSION_COOKIE}.${i}`, { path: '/' });
		}
	}

	const isPublic = PUBLIC_PREFIXES.some((p) => pathname === p || pathname.startsWith(p));
	if (!event.locals.user && !isPublic) {
		if (event.request.method !== 'GET' || event.request.headers.get('accept')?.includes('application/json')) {
			return harden(
				new Response(JSON.stringify({ code: 'Unauthenticated', message: 'Your session has expired. Please sign in again.' }), {
					status: 401,
					headers: { 'content-type': 'application/json' }
				}),
				cfg.secureCookies
			);
		}
		const to = `/auth/login?returnTo=${encodeURIComponent(pathname + event.url.search)}`;
		return harden(new Response(null, { status: 303, headers: { location: to } }), cfg.secureCookies);
	}

	return harden(await resolve(event), cfg.secureCookies);
};
