import { describe, expect, it } from 'vitest';
import { connectFromForm, fieldHelp, help, PROVIDERS } from './integrations';

const form = (o: Record<string, string>) => {
	const f = new FormData();
	for (const [k, v] of Object.entries(o)) f.set(k, v);
	return f;
};

describe('connect form', () => {
	it('reads credentials (always) and settings (only when filled)', () => {
		const r = connectFromForm(form({ c_token: ' secret_x ', s_database_id: 'abc', s_title_property: '  ' }), ['token'], ['database_id', 'title_property']);
		expect(r.credentials).toEqual({ token: 'secret_x' });
		expect(r.config).toEqual({ database_id: 'abc' });
	});
	it('sends a blank credential as blank so the server rejects it, not silently drops it', () => {
		expect(connectFromForm(form({}), ['token'], []).credentials).toEqual({ token: '' });
	});
	it('ignores fields the provider does not define', () => {
		const r = connectFromForm(form({ c_token: 'x', c_admin: 'yes', s_evil: '1' }), ['token'], []);
		expect(r.credentials).toEqual({ token: 'x' });
		expect(r.config).toEqual({});
	});
});

describe('provider wording', () => {
	it('marks every secret credential as a secret so it is rendered as a password field', () => {
		for (const p of Object.values(PROVIDERS)) {
			for (const [name, f] of Object.entries(p.credentials)) {
				if (/secret|token/.test(name)) expect(f.secret, name).toBe(true);
			}
		}
	});
	it('explains that people are never emailed without approval', () => {
		expect(help('google').agents).toMatch(/approval/i);
		expect(help('zoom').agents).toMatch(/approval/i);
	});
	it('falls back sensibly for fields it does not know', () => {
		expect(fieldHelp('credentials', 'google', 'new_secret_field')).toEqual({ label: 'new secret field', secret: true });
		expect(fieldHelp('settings', 'zoom', 'new_setting').secret).toBe(false);
		expect(help('mystery').name).toBe('mystery');
	});
});
