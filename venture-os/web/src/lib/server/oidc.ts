// OpenID Connect authorization-code flow with PKCE, run entirely on the server (backend-for-frontend).
import * as client from 'openid-client';
import { config } from './config';
import type { Session } from './session';

let discovered: Promise<client.Configuration> | undefined;

async function oidc(): Promise<client.Configuration> {
	if (!discovered) {
		const c = config();
		const issuer = new URL(c.oidcIssuer);
		// Plain HTTP is only tolerated for local development issuers.
		const local = issuer.protocol === 'http:';
		discovered = client
			.discovery(issuer, c.oidcClientId, undefined, client.None(), local ? { execute: [client.allowInsecureRequests] } : undefined)
			.catch((e) => {
				discovered = undefined; // retry on the next request (Keycloak may still be starting)
				throw e;
			});
	}
	return discovered;
}

export interface LoginState {
	state: string;
	nonce: string;
	verifier: string;
	returnTo: string;
}

export const LOGIN_COOKIE = 'venture_login';

function redirectUri(): string {
	return `${config().origin}/auth/callback`;
}

/** Begins a login: returns the provider URL and the state to remember (in a short-lived sealed cookie). */
export async function beginLogin(returnTo: string): Promise<{ url: URL; state: LoginState }> {
	const cfg = await oidc();
	const verifier = client.randomPKCECodeVerifier();
	const state: LoginState = { state: client.randomState(), nonce: client.randomNonce(), verifier, returnTo };
	const url = client.buildAuthorizationUrl(cfg, {
		redirect_uri: redirectUri(),
		scope: 'openid profile email',
		code_challenge: await client.calculatePKCECodeChallenge(verifier),
		code_challenge_method: 'S256',
		state: state.state,
		nonce: state.nonce
	});
	return { url, state };
}

function toSession(tokens: client.TokenEndpointResponse & client.TokenEndpointResponseHelpers, prev?: Session): Session {
	const claims = tokens.claims();
	const now = Math.floor(Date.now() / 1000);
	const sub = claims?.sub ?? prev?.sub;
	if (!sub) throw new Error('identity provider returned no subject');
	return {
		sub,
		name: String(claims?.name ?? claims?.preferred_username ?? prev?.name ?? sub),
		email: String(claims?.email ?? prev?.email ?? ''),
		at: tokens.access_token,
		rt: tokens.refresh_token ?? prev?.rt,
		exp: now + (tokens.expires_in ?? 300),
		iat: prev?.iat ?? now
	};
}

/** Completes a login from the provider's redirect. Throws on any state/nonce/PKCE/signature failure. */
export async function completeLogin(callbackUrl: URL, login: LoginState): Promise<Session> {
	const cfg = await oidc();
	const tokens = await client.authorizationCodeGrant(cfg, callbackUrl, {
		pkceCodeVerifier: login.verifier,
		expectedState: login.state,
		expectedNonce: login.nonce
	});
	return toSession(tokens);
}

/** Exchanges the refresh token for a fresh access token, keeping the original session start. */
export async function refresh(session: Session): Promise<Session | null> {
	if (!session.rt) return null;
	try {
		const cfg = await oidc();
		return toSession(await client.refreshTokenGrant(cfg, session.rt), session);
	} catch {
		return null;
	}
}

/** URL that ends the provider session and returns to the app. */
export async function logoutUrl(): Promise<URL | null> {
	const cfg = await oidc();
	const endpoint = cfg.serverMetadata().end_session_endpoint;
	if (!endpoint) return null;
	const u = new URL(endpoint);
	u.searchParams.set('client_id', config().oidcClientId);
	u.searchParams.set('post_logout_redirect_uri', config().origin + '/');
	return u;
}
