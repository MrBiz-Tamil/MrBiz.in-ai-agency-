// JSON endpoints behind the launch wizard. Each one validates its input, calls the control plane as the signed-in
// user, and returns plain JSON. The browser never sees a token and never talks to the control plane directly.
import { json } from '@sveltejs/kit';
import { api, toFailure } from '$lib/server/api';
import type { RequestHandler } from './$types';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const HEX64 = /^[0-9a-f]{64}$/i;

class Bad extends Error {}

function uuid(v: unknown, field: string): string {
	if (typeof v !== 'string' || !UUID.test(v)) throw new Bad(`${field} must be a uuid`);
	return v;
}
function key(v: unknown, field: string): string {
	if (typeof v !== 'string' || v.length < 8 || v.length > 120) throw new Bad(`${field} is required`);
	return v;
}
const text = (bytes: Uint8Array) => (bytes.length ? new TextDecoder().decode(bytes) : '');

function fail(e: unknown) {
	if (e instanceof Bad) return json({ code: 'InvalidArgument', message: e.message }, { status: 400 });
	const f = toFailure(e);
	return json({ code: f.code, message: f.message }, { status: f.status });
}

export const POST: RequestHandler = async ({ params, request, locals }) => {
	let body: Record<string, unknown>;
	try {
		body = await request.json();
		if (typeof body !== 'object' || body === null) throw new Bad('body must be an object');
	} catch {
		return json({ code: 'InvalidArgument', message: 'Request body must be JSON.' }, { status: 400 });
	}
	const a = api(locals.accessToken!);
	try {
		switch (params.action) {
			case 'start': {
				const created = await a.tenant.createBusiness({ idempotencyKey: key(body.key, 'key') });
				const s = await a.genesis.startSession({ tenantId: created.tenantId, idempotencyKey: 'wizard-v1' });
				return json({ tenantId: created.tenantId, sessionId: s.sessionId });
			}
			case 'converse': {
				const message = typeof body.message === 'string' ? body.message : '';
				if (!message.trim()) throw new Bad('Write a message first.');
				const r = await a.genesis.converse({
					tenantId: uuid(body.tenantId, 'tenantId'),
					sessionId: uuid(body.sessionId, 'sessionId'),
					message,
					turnKey: key(body.turnKey, 'turnKey')
				});
				return json({
					assistant: r.assistantMessage,
					draft: text(r.draftBlueprintJson),
					changed: r.draftChanged,
					ready: r.ready,
					problems: r.problems,
					replayed: r.replayed
				});
			}
			case 'submit': {
				const r = await a.genesis.submitBlueprint({
					tenantId: uuid(body.tenantId, 'tenantId'),
					sessionId: uuid(body.sessionId, 'sessionId'),
					idempotencyKey: key(body.key, 'key')
				});
				return json({ blueprintId: r.blueprintId, version: r.version, hash: r.contentHash, status: r.status });
			}
			case 'approve': {
				const hash = String(body.hash ?? '');
				if (!HEX64.test(hash)) throw new Bad('hash must be 64 hex characters');
				const r = await a.genesis.approveBlueprint({
					tenantId: uuid(body.tenantId, 'tenantId'),
					blueprintId: uuid(body.blueprintId, 'blueprintId'),
					contentHash: hash
				});
				return json({ status: r.status });
			}
			case 'provision': {
				const r = await a.provisioner.provision({
					tenantId: uuid(body.tenantId, 'tenantId'),
					blueprintId: uuid(body.blueprintId, 'blueprintId'),
					idempotencyKey: key(body.key, 'key')
				});
				return json({
					runId: r.runId,
					status: r.status,
					replayed: r.replayed,
					elapsedMs: Number(r.elapsedMs),
					steps: r.steps.map((s) => ({ key: s.key, status: s.status, attempts: s.attempts, error: s.error }))
				});
			}
			default:
				return json({ code: 'NotFound', message: 'Unknown action.' }, { status: 404 });
		}
	} catch (e) {
		return fail(e);
	}
};

export const GET: RequestHandler = async ({ params, url, locals }) => {
	if (params.action !== 'status') return json({ code: 'NotFound', message: 'Unknown action.' }, { status: 404 });
	try {
		const tenantId = uuid(url.searchParams.get('tenantId'), 'tenantId');
		const r = await api(locals.accessToken!).provisioner.getStatus({ tenantId });
		return json({
			runId: r.runId,
			status: r.status,
			error: r.error,
			steps: r.steps.map((s) => ({ key: s.key, status: s.status, attempts: s.attempts, error: s.error }))
		});
	} catch (e) {
		return fail(e);
	}
};
