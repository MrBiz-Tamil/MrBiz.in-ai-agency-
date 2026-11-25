// Web audit 2026-11-25: every post-login destination safeReturnTo accepts must resolve on OUR origin once a
// browser has parsed it. Browsers (WHATWG URL) delete tab, CR and LF anywhere in a URL and treat "\" like "/", so
// "/<TAB>/evil.example" is "//evil.example" to a browser even though it does not start with "//" as a string.
import { describe, expect, it } from 'vitest';
import { safeReturnTo } from './session';

const ORIGIN = 'https://app.example.test';

describe('safeReturnTo resolves on our own origin', () => {
	// Every ASCII control character and space, in the positions an attacker would try.
	const controls = Array.from({ length: 0x21 }, (_, i) => String.fromCharCode(i)).concat(['\x7f', '\u0085', ' ', ' ', ' ', '​', '﻿']);
	const candidates: string[] = [];
	for (const c of controls) {
		candidates.push(`/${c}/evil.example`, `/${c}\\evil.example`, `/${c}${c}/evil.example`, `/\\${c}evil.example`, `/${c}evil.example`, `${c}//evil.example`);
	}
	candidates.push('//evil.example', '/\\evil.example', '/\\/evil.example', '/%2f/evil.example', '/.//evil.example', 'https://evil.example', '/auth/login');

	it('never sends the browser to another origin', () => {
		const escaped: string[] = [];
		for (const raw of candidates) {
			const out = safeReturnTo(raw);
			if (new URL(out, ORIGIN).origin !== ORIGIN) escaped.push(JSON.stringify(raw));
		}
		expect(escaped).toEqual([]);
	});

	it('still allows ordinary in-app destinations unchanged', () => {
		const ok = ['/', '/approvals', '/approvals?status=EXECUTED&b=all', '/b/079d07c2-5ced-485b-a737-791dee210fa4/crm/deals/1', '/launch?b=abc', '/.//x'];
		for (const path of ok) expect(safeReturnTo(path)).toBe(path);
	});

	it('falls back to the home page for empty, absolute, protocol-relative, control-character and auth destinations', () => {
		const bad = [null, undefined, '', 'approvals', 'https://evil.example', '//evil.example', '/\\evil.example', '/\t/evil.example', '/\n/evil.example', '/\r/evil.example', '/auth/logout'];
		for (const raw of bad) expect(safeReturnTo(raw)).toBe('/');
	});
});
