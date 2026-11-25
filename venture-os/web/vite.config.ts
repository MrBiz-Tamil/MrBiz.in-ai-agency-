import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	server: { host: '127.0.0.1', port: Number(process.env.VITE_DEV_PORT ?? 5190), strictPort: true },
	test: { include: ['src/**/*.test.ts'], environment: 'node' }
});
