// What a founder needs to know to connect each provider: friendly labels, where to find each value, and what the
// connection lets agents do. The control plane is the authority on which fields exist (it sends them with List); this
// only adds wording, and falls back to the raw field name for anything it does not know.

export interface FieldHelp {
	label: string;
	help?: string;
	secret?: boolean;
}

export interface ProviderHelp {
	name: string;
	summary: string;
	/** What agents can do once connected, and what always needs a person. */
	agents: string;
	credentials: Record<string, FieldHelp>;
	settings: Record<string, FieldHelp>;
}

export const PROVIDERS: Record<string, ProviderHelp> = {
	google: {
		name: 'Google Calendar and Meet',
		summary: 'Check availability and create meetings with a Google Meet link on a Google Calendar.',
		agents: 'Agents can check when you are free and create meetings for themselves. Inviting people by email is always sent to you for approval first.',
		credentials: {
			client_id: { label: 'OAuth client ID', help: 'From a Google Cloud OAuth client with the Calendar API enabled.' },
			client_secret: { label: 'OAuth client secret', secret: true },
			refresh_token: { label: 'Refresh token', help: 'Obtained once by authorising the client with the calendar scope. It does not expire unless you revoke it.', secret: true }
		},
		settings: { calendar_id: { label: 'Calendar', help: 'Leave empty for the main calendar, or paste a calendar ID.' } }
	},
	zoom: {
		name: 'Zoom',
		summary: 'Create Zoom meetings and get their join links.',
		agents: 'Agents can create and cancel meetings. Zoom does not email invitations, so sharing the link is a separate step that needs your approval.',
		credentials: {
			account_id: { label: 'Account ID', help: 'From a Server-to-Server OAuth app in the Zoom App Marketplace.' },
			client_id: { label: 'Client ID' },
			client_secret: { label: 'Client secret', secret: true }
		},
		settings: { user_id: { label: 'Zoom user', help: 'Leave empty to use the app owner, or enter a user ID or email.' } }
	},
	notion: {
		name: 'Notion',
		summary: 'Write call notes and minutes into a Notion database, and find pages.',
		agents: 'Agents can create pages, add to them and search by title, but only in what you share with the integration.',
		credentials: { token: { label: 'Integration token', help: 'From a Notion internal integration. Share your notes database with it.', secret: true } },
		settings: {
			database_id: { label: 'Notes database ID', help: 'The 32-character ID in the database URL. The database must be shared with the integration.' },
			title_property: { label: 'Title column name', help: 'Leave empty if the database title column is called Name.' }
		}
	}
};

export function help(provider: string): ProviderHelp {
	return (
		PROVIDERS[provider] ?? { name: provider, summary: '', agents: '', credentials: {}, settings: {} }
	);
}

export function fieldHelp(kind: 'credentials' | 'settings', provider: string, field: string): FieldHelp {
	return help(provider)[kind][field] ?? { label: field.replace(/_/g, ' '), secret: kind === 'credentials' };
}

/** Reads a connect form: credentials and settings are posted as c_<name> and s_<name>; empty settings are omitted. */
export function connectFromForm(form: FormData, credentials: string[], settings: string[]) {
	const read = (prefix: string, names: string[], keepEmpty: boolean) => {
		const out: Record<string, string> = {};
		for (const n of names) {
			const v = String(form.get(`${prefix}${n}`) ?? '').trim();
			if (v !== '' || keepEmpty) out[n] = v;
		}
		return out;
	};
	return { credentials: read('c_', credentials, true), config: read('s_', settings, false) };
}
