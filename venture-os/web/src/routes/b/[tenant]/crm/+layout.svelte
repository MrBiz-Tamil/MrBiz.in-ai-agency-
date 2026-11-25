<script lang="ts">
	import { page } from '$app/state';
	import Badge from '$lib/components/Badge.svelte';

	let { data, children } = $props();
	const base = $derived(`/b/${data.business.id}/crm`);
	const tabs = $derived([
		{ href: base, label: 'Pipeline', match: (p: string) => p === base || p.startsWith(`${base}/deals`) },
		{ href: `${base}/companies`, label: 'Companies', match: (p: string) => p.startsWith(`${base}/companies`) },
		{ href: `${base}/tasks`, label: 'Tasks', match: (p: string) => p.startsWith(`${base}/tasks`) }
	]);
</script>

<svelte:head><title>CRM · {data.business.name} · Venture OS</title></svelte:head>

<nav class="mb-4 text-sm"><a href="/b/{data.business.id}" class="text-ink-2 hover:underline">← {data.business.name}</a></nav>

<div class="flex flex-wrap items-center gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">Sales</h1>
	<Badge value={data.business.status} />
	{#if !data.canWrite && !data.unavailable}<span class="rounded-full bg-surface-2 px-2 py-0.5 text-xs text-ink-2">View only</span>{/if}
</div>

{#if data.unavailable}
	<p class="mt-6 rounded-xl border border-line bg-surface p-5 text-sm text-ink-2" role="status">{data.unavailable}</p>
{:else}
	<nav class="mt-4 flex gap-1 border-b border-line" aria-label="Sales sections">
		{#each tabs as t (t.href)}
			<a
				href={t.href}
				class="-mb-px border-b-2 px-3 py-2 text-sm {t.match(page.url.pathname) ? 'border-accent font-medium text-ink' : 'border-transparent text-ink-2 hover:text-ink'}"
				aria-current={t.match(page.url.pathname) ? 'page' : undefined}>{t.label}</a
			>
		{/each}
	</nav>
	<div class="mt-5">{@render children()}</div>
{/if}
