/** @type {import('tailwindcss').Config} */
export default {
	content: ['./src/**/*.{html,js,svelte,ts}'],
	theme: {
		extend: {
			colors: {
				surface: {
					950: '#07090e',
					900: '#0d1117',
					800: '#161b22',
					700: '#21262d',
					600: '#30363d'
				}
			}
		}
	},
	plugins: []
};
