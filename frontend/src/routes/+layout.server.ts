import type { LayoutServerLoad } from './$types';

export const load: LayoutServerLoad = ({ locals }) => {
	const s = locals.userSession;
	// Only what the UI needs. The session token stays on the server.
	return {
		session: s ? { workspaceId: s.workspaceId, role: s.role, email: s.email, displayName: s.displayName } : null
	};
};
