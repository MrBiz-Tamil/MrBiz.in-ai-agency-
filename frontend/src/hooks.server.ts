import { dev } from '$app/environment';
import { env } from '$env/dynamic/public';
import { redirect, type Handle } from '@sveltejs/kit';
import { resolveSession, SESSION_COOKIE } from '$lib/server/backend';

// Pages reachable without a session.
const PUBLIC_PATHS = ['/login'];

function wsOrigin(): string {
	try {
		const u = new URL(env.PUBLIC_GRAPHQL_WS_URL || 'ws://127.0.0.1:8080/graphql');
		return `${u.protocol}//${u.host}`;
	} catch {
		return 'ws://127.0.0.1:8080';
	}
}

const CSP = [
	"default-src 'self'",
	"script-src 'self'",
	"style-src 'self' 'unsafe-inline'", // Svelte transitions/animations emit inline styles
	"img-src 'self' data:",
	"font-src 'self' data:",
	`connect-src 'self' ${wsOrigin()}`,
	"frame-ancestors 'none'",
	"base-uri 'self'",
	"form-action 'self'",
	"object-src 'none'"
].join('; ');

export const handle: Handle = async ({ event, resolve }) => {
	const { pathname } = event.url;
	const token = event.cookies.get(SESSION_COOKIE);

	// The session is validated against the backend on every request (briefly cached). Nothing in
	// the cookie is trusted by itself: it is an opaque random token.
	const user = token ? await resolveSession(token) : null;
	if (user && token) {
		event.locals.userSession = {
			userId: user.userId,
			workspaceId: user.workspaceId,
			email: user.email,
			displayName: user.displayName,
			role: user.role
		};
		event.locals.sessionToken = token;
	} else if (token) {
		event.cookies.delete(SESSION_COOKIE, { path: '/' }); // expired or revoked
	}

	if (!event.locals.userSession && !PUBLIC_PATHS.includes(pathname)) {
		const wantsHtml = event.request.method === 'GET' && (event.request.headers.get('accept') ?? '').includes('text/html');
		if (wantsHtml) redirect(303, `/login?next=${encodeURIComponent(pathname + event.url.search)}`);
		return new Response(JSON.stringify({ error: 'authentication required' }), {
			status: 401,
			headers: { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }
		});
	}

	const response = await resolve(event);

	response.headers.set('X-Frame-Options', 'DENY');
	response.headers.set('X-Content-Type-Options', 'nosniff');
	response.headers.set('Referrer-Policy', 'strict-origin-when-cross-origin');
	response.headers.set('Permissions-Policy', 'microphone=(self), camera=()');
	if (!dev) {
		response.headers.set('Strict-Transport-Security', 'max-age=63072000; includeSubDomains');
		response.headers.set('Content-Security-Policy', CSP);
	}
	return response;
};
