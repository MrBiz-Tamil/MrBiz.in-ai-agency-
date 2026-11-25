import { env } from '$env/dynamic/private';
import { building } from '$app/environment';
import { simulatedSystems } from '$lib/simulated';
import { deriveKey } from './session';

export interface AppConfig {
	authMode: 'oidc' | 'dev';
	controlplaneUrl: string;
	oidcIssuer: string;
	oidcClientId: string;
	origin: string;
	key: Buffer;
	/** Systems the control plane simulates, for the UI banner (empty when everything is real). */
	simulated: string[];
	secureCookies: boolean;
}

let cached: AppConfig | undefined;

/** Reads and validates configuration on first use, failing with a message that names what is missing. */
export function config(): AppConfig {
	if (cached) return cached;
	const authMode = (env.AUTH_MODE ?? 'oidc') as 'oidc' | 'dev';
	if (authMode !== 'oidc' && authMode !== 'dev') throw new Error('AUTH_MODE must be "oidc" or "dev"');
	if (authMode === 'dev' && process.env.NODE_ENV === 'production') {
		throw new Error('AUTH_MODE=dev bypasses authentication and is refused in production');
	}
	const missing: string[] = [];
	const need = (k: string) => {
		const v = env[k];
		if (!v) missing.push(k);
		return v ?? '';
	};
	const controlplaneUrl = need('CONTROLPLANE_URL');
	const origin = need('ORIGIN');
	const secret = need('SESSION_SECRET');
	const oidcIssuer = authMode === 'oidc' ? need('OIDC_ISSUER_URL') : '';
	if (missing.length) throw new Error(`Missing required environment: ${missing.join(', ')}`);

	cached = {
		authMode,
		controlplaneUrl: controlplaneUrl.replace(/\/$/, ''),
		oidcIssuer,
		oidcClientId: env.OIDC_CLIENT_ID ?? 'venture-web',
		origin: origin.replace(/\/$/, ''),
		key: deriveKey(secret),
		simulated: simulatedSystems((k) => env[k]),
		secureCookies: origin.startsWith('https://')
	};
	return cached;
}

export { building };
