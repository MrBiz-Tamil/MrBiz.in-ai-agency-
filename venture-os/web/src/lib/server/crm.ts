// Server-side access to a business's CRM, as the signed-in user. The control plane does the authorisation (viewer to
// read, approver or founder to write) and the validation; this only carries JSON to it and back.
import { Code, ConnectError } from '@connectrpc/connect';
import { fail } from '@sveltejs/kit';
import { forPeople, type Page } from '$lib/crm';
import { api, toFailure } from './api';

/** Runs one CRM operation and returns its JSON result. Throws a ConnectError on failure. */
export async function crmCall<T>(accessToken: string, tenant: string, operation: string, args: object = {}): Promise<T> {
	const r = await api(accessToken).crm.execute({
		tenantId: tenant,
		operation,
		argsJson: new TextEncoder().encode(JSON.stringify(args))
	});
	return JSON.parse(new TextDecoder().decode(r.resultJson)) as T;
}

/** Reads up to `maxPages` pages of 50, oldest first, and says whether there was more. */
export async function readAll<T>(
	fetchPage: (cursor?: string) => Promise<Page<T>>,
	maxPages = 4
): Promise<{ items: T[]; truncated: boolean }> {
	const items: T[] = [];
	let cursor: string | undefined;
	for (let i = 0; i < maxPages; i++) {
		const p = await fetchPage(cursor);
		items.push(...p.items);
		cursor = p.next_cursor;
		if (!cursor) return { items, truncated: false };
	}
	return { items, truncated: true };
}

/** True when the business simply has no CRM connected (as opposed to an error). */
export function isNoCrm(e: unknown): boolean {
	return e instanceof ConnectError && e.code === Code.FailedPrecondition && /no CRM connected/i.test(e.rawMessage);
}

/** A new idempotency key for a form. It is issued when the page loads, so a double-submit is one record, not two. */
export const newKey = () => crypto.randomUUID();

/**
 * Runs a write for a form action and shapes the outcome for the page: {ok:true, ...} or a failure carrying the
 * message to show. Failures are never swallowed and unexpected ones are not leaked (see toFailure).
 */
export async function act<T extends object = object>(
	accessToken: string,
	tenant: string,
	operation: string,
	args: object,
	then?: (result: T) => object
) {
	try {
		const result = await crmCall<T>(accessToken, tenant, operation, args);
		return { ok: true as const, ...(then ? then(result) : {}) };
	} catch (e) {
		const f = toFailure(e);
		return fail(f.status, { ok: false as const, message: forPeople(f.message) });
	}
}

const text = (form: FormData, k: string, max = 20000) => String(form.get(k) ?? '').trim().slice(0, max);

/** The "add to history" form (note, call, email, meeting or task) on a deal or a company. */
export function addActivity(accessToken: string, tenant: string, form: FormData) {
	const args: Record<string, string> = {
		kind: text(form, 'kind', 20),
		subject: text(form, 'subject', 200),
		idempotency_key: text(form, 'key', 200)
	};
	for (const k of ['body', 'due', 'deal_id', 'company_id']) if (text(form, k)) args[k] = text(form, k);
	return act(accessToken, tenant, 'crm_add_activity', args);
}

/** The "Done" button on a task. */
export function completeTask(accessToken: string, tenant: string, form: FormData) {
	return act(accessToken, tenant, 'crm_complete_task', { activity_id: text(form, 'activity_id', 100) });
}

/** The stage selector on a deal card or deal page. The version makes a stale move fail instead of overwriting. */
export function moveDeal(accessToken: string, tenant: string, form: FormData) {
	const v = Number(form.get('version'));
	return act(accessToken, tenant, 'crm_move_deal', {
		deal_id: text(form, 'deal_id', 100),
		stage: text(form, 'stage', 100),
		...(Number.isSafeInteger(v) && v > 0 ? { expected_version: v } : {})
	});
}
