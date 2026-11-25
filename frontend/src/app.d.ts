declare global {
	namespace App {
		interface Locals {
			userSession?: {
				userId: string;
				workspaceId: string;
				email: string;
				displayName: string;
				role: 'owner' | 'admin' | 'operator' | 'auditor';
			};
			/** The raw session token. Server-only: never put it in page data. */
			sessionToken?: string;
		}
		interface PageData {
			session: { workspaceId: string; role: string; email: string; displayName: string } | null;
		}
	}
}

export {};
