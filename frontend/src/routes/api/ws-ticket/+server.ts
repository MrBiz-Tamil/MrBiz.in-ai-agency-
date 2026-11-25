import { error, json } from '@sveltejs/kit';
import { backendFetch } from '$lib/server/backend';
import type { RequestHandler } from './$types';

/** Issues a 60-second single-use ticket so the browser can open a websocket without ever seeing the session token. */
export const POST: RequestHandler = async ({ request, locals, url }) => {
	if (!locals.sessionToken) error(401, 'Authentication required');
	const origin = request.headers.get('origin');
	if (origin && origin !== url.origin) error(403, 'Cross-origin request refused');

	let res: Response;
	try {
		res = await backendFetch('/auth/ws-ticket', { method: 'POST' }, locals.sessionToken, 5000);
	} catch {
		error(502, 'Backend unreachable');
	}
	if (!res.ok) error(res.status === 401 ? 401 : 502, 'Could not issue a ticket');
	const { ticket } = (await res.json()) as { ticket: string };
	return json({ ticket }, { headers: { 'Cache-Control': 'no-store' } });
};
