import { env } from '$env/dynamic/private';

export const SESSION_COOKIE = 'abos_session';

export function backendUrl(): string {
	return (env.ABOS_BACKEND_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
}

export interface BackendUser {
	userId: string;
	workspaceId: string;
	email: string;
	displayName: string;
	role: 'owner' | 'admin' | 'operator' | 'auditor';
}

/** fetch against the Go backend with a timeout. Never sends credentials unless a token is given. */
export async function backendFetch(path: string, init: RequestInit = {}, token?: string, timeoutMs = 10000): Promise<Response> {
	const headers = new Headers(init.headers);
	if (token) headers.set('Authorization', `Bearer ${token}`);
	const ctrl = new AbortController();
	const timer = setTimeout(() => ctrl.abort(), timeoutMs);
	try {
		return await fetch(`${backendUrl()}${path}`, { ...init, headers, signal: ctrl.signal, redirect: 'manual' });
	} finally {
		clearTimeout(timer);
	}
}

// Short-lived cache so each page load does not cost a backend round trip. A revoked session
// (logout, expiry) stops working within TTL_MS.
const TTL_MS = 15_000;
const MAX_ENTRIES = 1000;
const cache = new Map<string, { user: BackendUser | null; at: number }>();

export function forgetSession(token: string): void {
	cache.delete(token);
}

/** Resolves a session token to its user via the backend, or null if it is not valid. */
export async function resolveSession(token: string): Promise<BackendUser | null> {
	const hit = cache.get(token);
	if (hit && Date.now() - hit.at < TTL_MS) return hit.user;

	let user: BackendUser | null = null;
	try {
		const res = await backendFetch('/auth/session', {}, token, 5000);
		if (res.ok) user = ((await res.json()) as { user: BackendUser }).user;
		else if (res.status !== 401) return hit?.user ?? null; // backend trouble: do not cache a failure as "logged out"
	} catch {
		return hit?.user ?? null;
	}
	if (cache.size >= MAX_ENTRIES) cache.delete(cache.keys().next().value as string);
	cache.set(token, { user, at: Date.now() });
	return user;
}
