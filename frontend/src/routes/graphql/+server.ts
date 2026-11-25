import { error } from '@sveltejs/kit';
import { backendFetch } from '$lib/server/backend';
import type { RequestHandler } from './$types';

const MAX_BODY_BYTES = 1024 * 1024;

/**
 * Same-origin GraphQL proxy (BFF). The browser never holds the session token: it lives in an
 * HttpOnly cookie, and this route attaches it as a bearer token on the way to the backend.
 */
export const POST: RequestHandler = async ({ request, locals, url }) => {
	if (!locals.sessionToken) error(401, 'Authentication required');

	// CSRF: a browser always sends Origin on cross-site POSTs. JSON content-type additionally
	// forces a CORS preflight for any other site.
	const origin = request.headers.get('origin');
	if (origin && origin !== url.origin) error(403, 'Cross-origin request refused');
	if (!(request.headers.get('content-type') ?? '').startsWith('application/json')) error(415, 'Expected application/json');

	const body = await request.text();
	if (body.length > MAX_BODY_BYTES) error(413, 'Payload too large');

	let res: Response;
	try {
		res = await backendFetch('/graphql', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body }, locals.sessionToken, 60000);
	} catch {
		error(502, 'Backend unreachable');
	}

	return new Response(res.body, {
		status: res.status,
		headers: { 'Content-Type': res.headers.get('Content-Type') ?? 'application/json', 'Cache-Control': 'no-store' }
	});
};
