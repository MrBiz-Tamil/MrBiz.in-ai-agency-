import { env } from '$env/dynamic/private';
import type { RequestHandler } from './$types';

/** Server-side health probe of the Go backend (avoids CORS and keeps the backend URL private). */
export const GET: RequestHandler = async ({ fetch }) => {
	const upstream = (env.ABOS_BACKEND_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
	const ctrl = new AbortController();
	const timer = setTimeout(() => ctrl.abort(), 2500);
	try {
		const res = await fetch(`${upstream}/healthz`, { signal: ctrl.signal });
		return new Response(res.ok ? 'ok' : 'unhealthy', { status: res.ok ? 200 : 503, headers: { 'Cache-Control': 'no-store' } });
	} catch {
		return new Response('unreachable', { status: 503, headers: { 'Cache-Control': 'no-store' } });
	} finally {
		clearTimeout(timer);
	}
};
