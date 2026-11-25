import { describe, expect, it } from 'vitest';
import {
	CHUNK_SIZE,
	MAX_SESSION_SECONDS,
	chunk,
	deriveKey,
	join,
	needsRefresh,
	open,
	safeReturnTo,
	seal,
	withinLifetime,
	type Session
} from './session';

const key = deriveKey('a-test-secret-that-is-long-enough-0123456789');
const session: Session = { sub: 'u1', name: 'Ada', email: 'ada@example.com', at: 'access-token', rt: 'refresh-token', exp: 2_000_000_000, iat: 1_000_000_000 };

describe('seal / open', () => {
	it('round-trips', () => {
		expect(open<Session>(key, seal(key, session))).toEqual(session);
	});

	it('produces different ciphertext each time (random IV)', () => {
		expect(seal(key, session)).not.toBe(seal(key, session));
	});

	it('does not contain the plaintext', () => {
		const sealed = seal(key, session);
		expect(sealed).not.toContain('access-token');
		expect(Buffer.from(sealed, 'base64url').toString('utf8')).not.toContain('access-token');
	});

	it('rejects tampering anywhere in the value', () => {
		const sealed = seal(key, session);
		for (const i of [0, 5, 13, 20, 30, sealed.length - 2]) {
			const flipped = sealed.slice(0, i) + (sealed[i] === 'A' ? 'B' : 'A') + sealed.slice(i + 1);
			expect(open(key, flipped)).toBeNull();
		}
	});

	it('rejects a value sealed under another key, truncation and garbage', () => {
		const other = deriveKey('another-secret-that-is-long-enough-9876543210');
		expect(open(other, seal(key, session))).toBeNull();
		expect(open(key, seal(key, session).slice(0, 30))).toBeNull();
		for (const junk of ['', undefined, null, 'not base64 !!', 'AAAA']) expect(open(key, junk as string)).toBeNull();
	});

	it('refuses a weak secret', () => {
		expect(() => deriveKey('short')).toThrow(/32 characters/);
	});
});

describe('chunked cookies', () => {
	it('splits and rejoins a large value', () => {
		const big = 'x'.repeat(CHUNK_SIZE * 2 + 100);
		const parts = chunk('s', big);
		expect(parts.map((p) => p.name)).toEqual(['s.0', 's.1', 's.2']);
		expect(parts.every((p) => p.value.length <= CHUNK_SIZE)).toBe(true);
		const jar = new Map(parts.map((p) => [p.name, p.value]));
		expect(join('s', (n) => jar.get(n))).toBe(big);
	});

	it('returns undefined when nothing is stored, and stops at a gap', () => {
		expect(join('s', () => undefined)).toBeUndefined();
		const jar = new Map([['s.0', 'a'], ['s.2', 'c']]);
		expect(join('s', (n) => jar.get(n))).toBe('a');
	});

	it('refuses values that would need too many cookies', () => {
		expect(() => chunk('s', 'x'.repeat(CHUNK_SIZE * 9))).toThrow(/too large/);
	});

	it('a real session fits in two cookies', () => {
		const withBigToken = { ...session, at: 'a'.repeat(1800), rt: 'r'.repeat(1200) };
		expect(chunk('s', seal(key, withBigToken)).length).toBeLessThanOrEqual(2);
	});
});

describe('lifetime', () => {
	it('expires the session after the absolute lifetime', () => {
		expect(withinLifetime(session, session.iat + MAX_SESSION_SECONDS - 1)).toBe(true);
		expect(withinLifetime(session, session.iat + MAX_SESSION_SECONDS)).toBe(false);
	});

	it('asks for a refresh shortly before the access token expires', () => {
		expect(needsRefresh(session, session.exp - 60)).toBe(false);
		expect(needsRefresh(session, session.exp - 30)).toBe(true);
		expect(needsRefresh(session, session.exp + 1)).toBe(true);
	});
});

describe('safeReturnTo (no open redirects)', () => {
	it('keeps same-site paths', () => {
		expect(safeReturnTo('/approvals?status=EXECUTED')).toBe('/approvals?status=EXECUTED');
		expect(safeReturnTo('/b/123')).toBe('/b/123');
	});

	it.each([
		['absolute URL', 'https://evil.example/'],
		['protocol-relative', '//evil.example'],
		['backslash trick', '/\\evil.example'],
		['no leading slash', 'evil.example'],
		['javascript scheme', 'javascript:alert(1)'],
		['header injection', '/ok\r\nSet-Cookie: x=1'],
		['auth loop', '/auth/login'],
		['empty', '']
	])('rejects %s', (_name, v) => {
		expect(safeReturnTo(v)).toBe('/');
	});

	it('handles missing input', () => {
		expect(safeReturnTo(null)).toBe('/');
		expect(safeReturnTo(undefined)).toBe('/');
	});
});
