import { createClient, fetchExchange, subscriptionExchange } from '@urql/svelte';
import { createClient as createWSClient } from 'graphql-ws';
import { browser } from '$app/environment';
import { env } from '$env/dynamic/public';

// Queries and mutations go through the same-origin /graphql proxy (which attaches the session).
// Subscriptions connect straight to the backend with a short-lived single-use ticket.
const WS_URL = env.PUBLIC_GRAPHQL_WS_URL || 'ws://127.0.0.1:8080/graphql';

async function fetchTicket(): Promise<string> {
	const res = await fetch('/api/ws-ticket', { method: 'POST', credentials: 'same-origin' });
	if (!res.ok) throw new Error('could not obtain a websocket ticket');
	return ((await res.json()) as { ticket: string }).ticket;
}

export function makeGraphQLClient() {
	const wsClient = browser
		? createWSClient({
				url: WS_URL,
				lazy: true,
				retryAttempts: Infinity,
				// A fresh ticket for every (re)connection: tickets are single use.
				connectionParams: async () => ({ ticket: await fetchTicket() })
			})
		: null;

	return createClient({
		url: '/graphql',
		fetchOptions: { credentials: 'same-origin' },
		exchanges: [
			fetchExchange,
			subscriptionExchange({
				forwardSubscription: (operation) => ({
					subscribe: (sink) => {
						if (!wsClient) return { unsubscribe: () => {} };
						const dispose = wsClient.subscribe(operation as any, sink as any);
						return { unsubscribe: dispose };
					}
				})
			})
		]
	});
}
