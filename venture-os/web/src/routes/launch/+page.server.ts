import { error } from '@sveltejs/kit';
import { api, toFailure } from '$lib/server/api';
import type { PageServerLoad } from './$types';

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** With ?b=<tenant>, resumes that business's interview (the session id is stable per tenant). */
export const load: PageServerLoad = async ({ locals, url }) => {
	const tenantId = url.searchParams.get('b');
	if (!tenantId) return { resume: null };
	if (!UUID.test(tenantId)) error(400, 'That business link is not valid.');
	const a = api(locals.accessToken!);
	try {
		const started = await a.genesis.startSession({ tenantId, idempotencyKey: 'wizard-v1' });
		const session = await a.genesis.getSession({ tenantId, sessionId: started.sessionId });
		return {
			resume: {
				tenantId,
				sessionId: started.sessionId,
				turns: session.turns.map((t) => ({ role: t.role, content: t.content })),
				draft: session.draftBlueprintJson.length ? new TextDecoder().decode(session.draftBlueprintJson) : ''
			}
		};
	} catch (e) {
		const f = toFailure(e);
		error(f.status, { message: f.message, code: f.code });
	}
};
