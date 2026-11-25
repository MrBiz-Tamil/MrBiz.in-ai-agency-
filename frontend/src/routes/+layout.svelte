<script lang="ts">
	import '../app.css';
	import { page } from '$app/stores';
	import Navbar from '$lib/components/Navbar.svelte';
	import VoiceWidget from '$lib/components/VoiceWidget.svelte';
	import { setContextClient } from '@urql/svelte';
	import { makeGraphQLClient } from '$lib/graphql/client';

	let { children, data } = $props();

	// One GraphQL client for all child routes.
	setContextClient(makeGraphQLClient());
</script>

<div class="min-h-screen bg-surface-950 flex flex-col font-sans relative">
	{#if data.session}
		<Navbar session={data.session} />
	{/if}
	<main class="flex-1 flex flex-col">
		{@render children()}
	</main>
	{#if data.session && $page.url.pathname !== '/login'}
		<VoiceWidget />
	{/if}
</div>
