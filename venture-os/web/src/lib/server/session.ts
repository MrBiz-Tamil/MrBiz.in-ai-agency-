// Encrypted session cookies. The session (user + control-plane tokens) lives ONLY in an AES-256-GCM sealed,
// httpOnly cookie: the browser can neither read nor forge it, and tokens are never present in page data.
// Sealed values are split across cookies when they would exceed browser limits (~4 KB per cookie).
import { createCipheriv, createDecipheriv, hkdfSync, randomBytes } from 'node:crypto';

export interface Session {
	sub: string;
	name: string;
	email: string;
	/** Control-plane bearer token (OIDC access token). */
	at: string;
	/** OIDC refresh token, when the provider issued one. */
	rt?: string;
	/** Access token expiry, epoch seconds. */
	exp: number;
	/** Session start, epoch seconds: bounds how long refreshing may extend a session. */
	iat: number;
}

export const SESSION_COOKIE = 'venture_session';
export const MAX_SESSION_SECONDS = 8 * 60 * 60;
export const CHUNK_SIZE = 3500;
const MAX_CHUNKS = 8;

export function deriveKey(secret: string): Buffer {
	if (secret.length < 32) throw new Error('SESSION_SECRET must be at least 32 characters');
	return Buffer.from(hkdfSync('sha256', secret, 'venture-os-web', 'session-cookie-v1', 32));
}

/** Encrypts and authenticates `data`. Output is base64url(iv | tag | ciphertext). */
export function seal(key: Buffer, data: unknown): string {
	const iv = randomBytes(12);
	const cipher = createCipheriv('aes-256-gcm', key, iv);
	const ct = Buffer.concat([cipher.update(JSON.stringify(data), 'utf8'), cipher.final()]);
	return Buffer.concat([iv, cipher.getAuthTag(), ct]).toString('base64url');
}

/** Returns the decrypted value, or null for anything tampered, truncated or encrypted under another key. */
export function open<T>(key: Buffer, sealed: string | undefined | null): T | null {
	if (!sealed) return null;
	try {
		const raw = Buffer.from(sealed, 'base64url');
		if (raw.length < 12 + 16 + 2) return null;
		const decipher = createDecipheriv('aes-256-gcm', key, raw.subarray(0, 12));
		decipher.setAuthTag(raw.subarray(12, 28));
		const pt = Buffer.concat([decipher.update(raw.subarray(28)), decipher.final()]);
		return JSON.parse(pt.toString('utf8')) as T;
	} catch {
		return null;
	}
}

/** Splits a sealed value into numbered cookies: name.0, name.1, ... */
export function chunk(name: string, value: string, size = CHUNK_SIZE): { name: string; value: string }[] {
	const parts: { name: string; value: string }[] = [];
	for (let i = 0, n = 0; i < value.length; i += size, n++) {
		parts.push({ name: `${name}.${n}`, value: value.slice(i, i + size) });
	}
	if (parts.length > MAX_CHUNKS) throw new Error('session too large to store in cookies');
	return parts;
}

/** Reassembles a chunked value; undefined if the first chunk is missing. */
export function join(name: string, get: (cookie: string) => string | undefined): string | undefined {
	let out = '';
	for (let n = 0; n < MAX_CHUNKS; n++) {
		const part = get(`${name}.${n}`);
		if (part === undefined) break;
		out += part;
	}
	return out === '' ? undefined : out;
}

/** True if the session may still be used (not older than the absolute lifetime). */
export function withinLifetime(s: Session, nowSec = Math.floor(Date.now() / 1000)): boolean {
	return nowSec - s.iat < MAX_SESSION_SECONDS;
}

/** True if the access token is expired or about to be. */
export function needsRefresh(s: Session, nowSec = Math.floor(Date.now() / 1000), skewSec = 30): boolean {
	return s.exp - skewSec <= nowSec;
}

/** Allows only same-site relative paths as post-login destinations (no open redirects). */
export function safeReturnTo(raw: string | null | undefined): string {
	if (!raw || !raw.startsWith('/') || raw.startsWith('//') || raw.startsWith('/\\') || /[\r\n]/.test(raw)) return '/';
	if (raw.startsWith('/auth/')) return '/';
	return raw;
}
