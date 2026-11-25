// Exercises the real openid-client flow in oidc.ts against an in-process OIDC provider that enforces PKCE,
// state, nonce, audience and signatures, the same checks Keycloak makes.
import { createHash, createSign, generateKeyPairSync } from 'node:crypto';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';

// SvelteKit's $env module reads a build-time snapshot, so tests supply their own mutable environment.
const mockEnv = vi.hoisted(() => ({}) as Record<string, string>);
vi.mock('$env/dynamic/private', () => ({ env: mockEnv }));
vi.mock('$app/environment', () => ({ building: false }));

const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 2048 });
const jwk = { ...publicKey.export({ format: 'jwk' }), kid: 'k1', alg: 'RS256', use: 'sig' };
const b64 = (b: Buffer | string) => Buffer.from(b).toString('base64url');

function signJwt(claims: Record<string, unknown>, key = privateKey): string {
	const head = b64(JSON.stringify({ alg: 'RS256', typ: 'JWT', kid: 'k1' }));
	const body = b64(JSON.stringify(claims));
	const sig = createSign('RSA-SHA256').update(`${head}.${body}`).sign(key);
	return `${head}.${body}.${b64(sig)}`;
}

interface Pending {
	challenge: string;
	nonce: string;
}
let server: Server;
let issuer = '';
const codes = new Map<string, Pending>();
let tokenCalls = 0;
let tamperAudience = false;

beforeAll(async () => {
	server = createServer((req, res) => {
		const url = new URL(req.url ?? '/', issuer || 'http://x');
		const json = (o: unknown, status = 200) => {
			res.writeHead(status, { 'content-type': 'application/json' });
			res.end(JSON.stringify(o));
		};
		if (url.pathname === '/.well-known/openid-configuration') {
			return json({
				issuer, authorization_endpoint: `${issuer}/auth`, token_endpoint: `${issuer}/token`, jwks_uri: `${issuer}/jwks`,
				end_session_endpoint: `${issuer}/logout`, id_token_signing_alg_values_supported: ['RS256'],
				code_challenge_methods_supported: ['S256'], response_types_supported: ['code'], subject_types_supported: ['public']
			});
		}
		if (url.pathname === '/jwks') return json({ keys: [jwk] });
		if (url.pathname === '/token' && req.method === 'POST') {
			let body = '';
			req.on('data', (c) => (body += c));
			req.on('end', () => {
				tokenCalls++;
				const p = new URLSearchParams(body);
				const now = Math.floor(Date.now() / 1000);
				const grant = p.get('grant_type');
				if (grant === 'refresh_token') {
					return p.get('refresh_token') === 'rt-1'
						? json({ access_token: 'at-2', refresh_token: 'rt-2', token_type: 'Bearer', expires_in: 300 })
						: json({ error: 'invalid_grant' }, 400);
				}
				const pending = codes.get(p.get('code') ?? '');
				codes.delete(p.get('code') ?? ''); // codes are single-use
				const verifierOk = pending && createHash('sha256').update(p.get('code_verifier') ?? '').digest('base64url') === pending.challenge;
				if (!pending || !verifierOk) return json({ error: 'invalid_grant' }, 400);
				json({
					access_token: 'at-1', refresh_token: 'rt-1', token_type: 'Bearer', expires_in: 300,
					id_token: signJwt({ iss: issuer, sub: 'user-42', aud: tamperAudience ? 'someone-else' : 'venture-web', exp: now + 300, iat: now, nonce: pending.nonce, name: 'Ada Lovelace', email: 'ada@example.com' })
				});
			});
			return;
		}
		res.writeHead(404).end();
	});
	await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
	issuer = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;

	Object.assign(mockEnv, {
		AUTH_MODE: 'oidc',
		OIDC_ISSUER_URL: issuer,
		OIDC_CLIENT_ID: 'venture-web',
		CONTROLPLANE_URL: 'http://127.0.0.1:1',
		ORIGIN: 'http://127.0.0.1:5190',
		SESSION_SECRET: 'a-test-secret-that-is-long-enough-0123456789'
	});
});

afterAll(() => new Promise<void>((r) => server.close(() => r())));

/** Plays the browser + provider: follows the authorize URL and returns the callback URL the provider would redirect to. */
function authorize(authUrl: URL, code = 'code-' + Math.random().toString(36).slice(2), overrides: Partial<{ state: string }> = {}): URL {
	expect(authUrl.origin).toBe(issuer);
	expect(authUrl.searchParams.get('code_challenge_method')).toBe('S256');
	expect(authUrl.searchParams.get('response_type')).toBe('code');
	expect(authUrl.searchParams.get('client_id')).toBe('venture-web');
	expect(authUrl.searchParams.get('redirect_uri')).toBe('http://127.0.0.1:5190/auth/callback');
	expect(authUrl.searchParams.get('scope')).toContain('openid');
	codes.set(code, { challenge: authUrl.searchParams.get('code_challenge')!, nonce: authUrl.searchParams.get('nonce')! });
	const cb = new URL('http://127.0.0.1:5190/auth/callback');
	cb.searchParams.set('code', code);
	cb.searchParams.set('state', overrides.state ?? authUrl.searchParams.get('state')!);
	cb.searchParams.set('iss', issuer);
	return cb;
}

describe('OIDC authorization-code flow with PKCE', () => {
	it('completes a login and builds the session from the tokens', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/approvals');
		expect(state.returnTo).toBe('/approvals');
		const session = await completeLogin(authorize(url), state);
		expect(session).toMatchObject({ sub: 'user-42', name: 'Ada Lovelace', email: 'ada@example.com', at: 'at-1', rt: 'rt-1' });
		expect(session.exp).toBeGreaterThan(Date.now() / 1000);
		expect(session.iat).toBeLessThanOrEqual(Math.floor(Date.now() / 1000));
	});

	it('uses a fresh state, nonce and PKCE verifier every time', async () => {
		const { beginLogin } = await import('./oidc');
		const a = await beginLogin('/');
		const b = await beginLogin('/');
		expect(a.state.state).not.toBe(b.state.state);
		expect(a.state.nonce).not.toBe(b.state.nonce);
		expect(a.state.verifier).not.toBe(b.state.verifier);
	});

	it('rejects a callback whose state does not match (CSRF)', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		await expect(completeLogin(authorize(url, undefined, { state: 'attacker-state' }), state)).rejects.toThrow();
	});

	it('rejects a code redeemed with the wrong PKCE verifier (code interception)', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		await expect(completeLogin(authorize(url), { ...state, verifier: 'x'.repeat(43) })).rejects.toThrow();
	});

	it('rejects an authorization code used twice (replay)', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		const cb = authorize(url, 'single-use');
		await completeLogin(cb, state);
		await expect(completeLogin(cb, state)).rejects.toThrow();
	});

	it('rejects an id token whose nonce does not match (token replay)', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		await expect(completeLogin(authorize(url), { ...state, nonce: 'some-other-nonce' })).rejects.toThrow();
	});

	it('rejects an id token issued for another client', async () => {
		const { beginLogin, completeLogin } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		tamperAudience = true;
		try {
			await expect(completeLogin(authorize(url), state)).rejects.toThrow();
		} finally {
			tamperAudience = false;
		}
	});

	it('refreshes tokens while keeping the original session start', async () => {
		const { beginLogin, completeLogin, refresh } = await import('./oidc');
		const { url, state } = await beginLogin('/');
		const first = await completeLogin(authorize(url), state);
		const before = tokenCalls;
		const renewed = await refresh({ ...first, iat: 12345 });
		expect(tokenCalls).toBe(before + 1);
		expect(renewed).toMatchObject({ sub: 'user-42', name: 'Ada Lovelace', at: 'at-2', rt: 'rt-2', iat: 12345 });
	});

	it('returns null when the refresh token is refused or absent', async () => {
		const { refresh } = await import('./oidc');
		const base = { sub: 's', name: 'n', email: 'e', at: 'a', exp: 0, iat: 1 };
		expect(await refresh({ ...base, rt: 'revoked' })).toBeNull();
		expect(await refresh(base)).toBeNull();
	});

	it('builds the provider logout URL with a return address', async () => {
		const { logoutUrl } = await import('./oidc');
		const u = await logoutUrl();
		expect(u?.pathname).toBe('/logout');
		expect(u?.searchParams.get('client_id')).toBe('venture-web');
		expect(u?.searchParams.get('post_logout_redirect_uri')).toBe('http://127.0.0.1:5190/');
	});
});
