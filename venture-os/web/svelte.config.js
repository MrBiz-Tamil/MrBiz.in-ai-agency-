import adapter from '@sveltejs/adapter-node';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

const prod = process.env.NODE_ENV === 'production';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		adapter: adapter(),
		// Tokens never reach the browser; this is the BFF's hardening: strict CSP (nonces for SvelteKit's own
		// bootstrap script), no framing, no third-party origins.
		csp: prod
			? {
					mode: 'auto',
					directives: {
						'default-src': ['self'],
						'script-src': ['self'],
						'style-src': ['self', 'unsafe-inline'],
						'img-src': ['self', 'data:'],
						'connect-src': ['self'],
						'font-src': ['self'],
						'object-src': ['none'],
						'base-uri': ['self'],
						'form-action': ['self'],
						'frame-ancestors': ['none']
					}
				}
			: undefined
		// CSRF origin checking for form posts is on by default in SvelteKit; hooks.server.ts adds an explicit
		// Origin check for every state-changing request as well.
	}
};

export default config;
