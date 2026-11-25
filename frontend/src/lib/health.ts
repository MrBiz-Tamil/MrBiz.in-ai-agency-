export type Health = 'HEALTHY' | 'CRITICAL' | 'UNKNOWN';

/** Probes the backend through the same-origin server route. Never throws. */
export async function probeBackend(timeoutMs = 4000): Promise<Health> {
	const ctrl = new AbortController();
	const timer = setTimeout(() => ctrl.abort(), timeoutMs);
	try {
		const res = await fetch('/api/backend-health', { signal: ctrl.signal, cache: 'no-store' });
		return res.ok ? 'HEALTHY' : 'CRITICAL';
	} catch {
		return 'CRITICAL';
	} finally {
		clearTimeout(timer);
	}
}
