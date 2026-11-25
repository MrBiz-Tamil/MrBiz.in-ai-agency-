declare global {
	namespace App {
		interface Locals {
			/** The signed-in user, or null on public routes. */
			user: { sub: string; name: string; email: string } | null;
			/** Bearer token for the control plane. Server-side only: never serialised to the browser. */
			accessToken: string | null;
			/** CSP nonce is handled by SvelteKit; kept for future use. */
		}
		interface Error {
			message: string;
			code?: string;
		}
	}
}

export {};
