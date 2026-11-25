import { error, fail } from '@sveltejs/kit';
import { decimalToMinor } from '$lib/crm';
import { minorDigits } from '$lib/format';
import { connectFromForm } from '$lib/integrations';
import { api, toFailure } from '$lib/server/api';
import type { Actions, PageServerLoad } from './$types';

const num = (v: bigint | undefined) => (v === undefined ? null : v);

export const load: PageServerLoad = async ({ locals, params, parent }) => {
	const { businesses } = await parent();
	const business = businesses.find((b) => b.id === params.tenant);
	if (!business) error(404, 'We could not find that business.');
	const base = { business, canManage: business.role === 'founder' };
	if (business.role === 'viewer') {
		return { ...base, restricted: true as const, providers: [], connections: [], caps: [] };
	}
	const a = api(locals.accessToken!);
	try {
		const [conns, caps] = await Promise.all([a.connection.list({ tenantId: params.tenant }), a.controls.getSpendCaps({ tenantId: params.tenant })]);
		return {
			...base,
			restricted: false as const,
			providers: conns.providers.map((p) => ({ provider: p.provider, credentials: [...p.credentials], settings: [...p.settings] })),
			connections: conns.connections.map((c) => ({ provider: c.provider, status: c.status, config: { ...c.config }, connectedBy: c.connectedBy, updatedAt: c.updatedAt })),
			caps: caps.caps.map((c) => ({
				currency: c.currency,
				perTransaction: num(c.perTransactionMinor),
				perHour: num(c.perHourMinor),
				perDay: num(c.perDayMinor),
				updatedAt: c.updatedAt
			}))
		};
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};

export const actions: Actions = {
	connect: async ({ locals, params, request }) => {
		const form = await request.formData();
		const provider = String(form.get('provider') ?? '');
		try {
			const a = api(locals.accessToken!);
			const fields = (await a.connection.list({ tenantId: params.tenant })).providers.find((p) => p.provider === provider);
			if (!fields) return fail(400, { ok: false as const, message: 'Choose a provider.' });
			const { credentials, config } = connectFromForm(form, [...fields.credentials], [...fields.settings]);
			await a.connection.connect({ tenantId: params.tenant, provider, config, credentials });
			// Nothing the person typed is returned: only the outcome.
			return { ok: true as const, connected: provider };
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	},

	disconnect: async ({ locals, params, request }) => {
		const provider = String((await request.formData()).get('provider') ?? '');
		try {
			await api(locals.accessToken!).connection.disconnect({ tenantId: params.tenant, provider });
			return { ok: true as const, disconnected: provider };
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	},

	setCap: async ({ locals, params, request }) => {
		const form = await request.formData();
		const currency = String(form.get('currency') ?? '').trim().toUpperCase();
		if (!/^[A-Z]{3}$/.test(currency)) return fail(400, { ok: false as const, message: 'Enter a 3-letter currency such as INR or USD.' });
		const digits = minorDigits(currency);
		const read = (name: string): bigint | undefined | null => {
			const raw = String(form.get(name) ?? '').trim();
			if (raw === '') return undefined; // no ceiling on this dimension
			return decimalToMinor(raw, digits); // null when it is not a valid amount
		};
		const [perTransactionMinor, perHourMinor, perDayMinor] = [read('perTransaction'), read('perHour'), read('perDay')];
		if ([perTransactionMinor, perHourMinor, perDayMinor].some((v) => v === null)) {
			return fail(400, { ok: false as const, message: 'Enter each limit like 50000 or 50,000.00, or leave it empty for no limit.' });
		}
		try {
			await api(locals.accessToken!).controls.setSpendCap({
				tenantId: params.tenant,
				cap: { currency, perTransactionMinor: perTransactionMinor ?? undefined, perHourMinor: perHourMinor ?? undefined, perDayMinor: perDayMinor ?? undefined }
			});
			return { ok: true as const, capSaved: currency };
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	},

	removeCap: async ({ locals, params, request }) => {
		const currency = String((await request.formData()).get('currency') ?? '');
		try {
			await api(locals.accessToken!).controls.setSpendCap({ tenantId: params.tenant, cap: { currency } });
			return { ok: true as const, capRemoved: currency };
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	},

	verifyAudit: async ({ locals, params }) => {
		try {
			const v = await api(locals.accessToken!).controls.verifyAudit({ tenantId: params.tenant });
			return { ok: true as const, audit: { intact: v.intact, firstBadId: String(v.firstBadId), rows: String(v.rowsChecked), head: v.headHex } };
		} catch (e) {
			const f = toFailure(e);
			return fail(f.status, { ok: false as const, message: f.message });
		}
	}
};
