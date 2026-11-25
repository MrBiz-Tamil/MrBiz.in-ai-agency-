import { redirect } from '@sveltejs/kit';
import type { PageServerLoad } from './$types';

// Redirect on the server so "/" never renders a blank page while client JS boots.
export const load: PageServerLoad = () => {
	redirect(307, '/operations');
};
