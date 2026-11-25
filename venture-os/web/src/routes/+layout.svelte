<script lang="ts">
	import '../app.css';
	import { page } from '$app/state';
	import BusinessSwitcher from '$lib/components/BusinessSwitcher.svelte';
	import CommandPalette from '$lib/components/CommandPalette.svelte';
	import type { PaletteItem } from '$lib/palette';
	import { formatMoney } from '$lib/format';

	let { data, children } = $props();
	let paletteOpen = $state(false);

	const pendingTotal = $derived(data.businesses.reduce((n, b) => n + b.pending, 0));

	const items = $derived<PaletteItem[]>([
		{ id: 'nav-portfolio', group: 'Go to', label: 'Portfolio overview', href: '/', keywords: 'home dashboard businesses' },
		{ id: 'nav-approvals', group: 'Go to', label: 'Approval inbox', href: '/approvals', hint: pendingTotal ? `${pendingTotal} pending` : undefined, keywords: 'approve reject queue' },
		{ id: 'nav-launch', group: 'Go to', label: 'Launch a new business', href: '/launch', keywords: 'create new wizard architect' },
		...data.businesses
			.filter((b) => b.status === 'ACTIVE')
			.map((b) => ({ id: `crm-${b.id}`, group: 'Sales', label: `Sales pipeline · ${b.name}`, href: `/b/${b.id}/crm`, keywords: 'crm deals companies customers leads tasks' })),
		...data.businesses.map((b) => ({
			id: `biz-${b.id}`,
			group: 'Businesses',
			label: b.name,
			hint: b.status === 'ACTIVE' ? 'Open' : b.status === 'DRAFT' ? 'Continue setup' : b.status.toLowerCase(),
			href: b.status === 'DRAFT' ? `/launch?b=${b.id}` : `/b/${b.id}`,
			keywords: `${b.slug} ${b.status}`
		})),
		...data.inbox.map((a) => ({
			id: `apr-${a.id}`,
			group: 'Pending approvals',
			label: a.title,
			hint: `${a.business}${a.amount !== null ? ' · ' + formatMoney(a.amount, a.currency) : ''}`,
			href: `/approvals/${a.tenantId}/${a.id}`,
			keywords: `${a.kind} ${a.risk} ${a.requestedBy} ${a.summary}`
		})),
		{ id: 'sign-out', group: 'Account', label: 'Sign out', run: () => (document.getElementById('signout') as HTMLFormElement | null)?.requestSubmit() }
	]);

	// Sales opens the selected business's CRM, or the first live business when "all" is selected.
	const salesTarget = $derived(data.businesses.find((b) => b.id === data.current && b.status === 'ACTIVE') ?? data.businesses.find((b) => b.status === 'ACTIVE'));
	const nav = $derived([
		{ href: '/', label: 'Portfolio', match: (p: string) => p === '/' || (p.startsWith('/b/') && !p.includes('/crm')) },
		...(salesTarget ? [{ href: `/b/${salesTarget.id}/crm`, label: 'Sales', match: (p: string) => p.includes('/crm') }] : []),
		{ href: '/approvals', label: 'Approvals', match: (p: string) => p.startsWith('/approvals') },
		{ href: '/launch', label: 'Launch', match: (p: string) => p.startsWith('/launch') }
	]);
</script>

<svelte:head><title>Venture OS</title></svelte:head>

{#if data.user}
	<a href="#main" class="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50 focus:rounded focus:bg-accent focus:px-3 focus:py-2 focus:text-accent-ink">Skip to content</a>
	<header class="sticky top-0 z-30 border-b border-line bg-surface/90 backdrop-blur">
		<div class="mx-auto flex max-w-7xl flex-wrap items-center gap-x-3 gap-y-2 px-4 py-2.5">
			<a href="/" class="flex items-center gap-2 font-semibold tracking-tight">
				<span class="grid size-7 place-items-center rounded-lg bg-accent text-accent-ink" aria-hidden="true">
					<svg viewBox="0 0 24 24" class="size-4" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><path d="M6 8l6 10 6-10" /></svg>
				</span>
				<span class="hidden sm:inline">Venture OS</span>
			</a>
			<BusinessSwitcher businesses={data.businesses} current={data.current} />
			<nav class="order-last flex w-full items-center gap-1 overflow-x-auto sm:order-none sm:ml-2 sm:w-auto" aria-label="Main">
				{#each nav as n (n.href)}
					<a
						href={n.href}
						class="relative rounded-lg px-3 py-1.5 text-sm {n.match(page.url.pathname) ? 'bg-surface-2 font-medium text-ink' : 'text-ink-2 hover:bg-surface-2'}"
						aria-current={n.match(page.url.pathname) ? 'page' : undefined}
					>
						{n.label}
						{#if n.label === 'Approvals' && pendingTotal > 0}
							<span class="ml-1 rounded-full bg-accent px-1.5 py-0.5 text-[11px] font-semibold text-accent-ink" aria-label="{pendingTotal} pending">{pendingTotal}</span>
						{/if}
					</a>
				{/each}
			</nav>
			<div class="ml-auto flex items-center gap-2">
				<button type="button" class="hidden items-center gap-2 rounded-lg border border-line bg-surface px-3 py-1.5 text-sm text-ink-3 hover:bg-surface-2 sm:flex" onclick={() => (paletteOpen = true)}>
					Search or jump to…
					<kbd class="rounded border border-line px-1.5 font-mono text-[11px]">Ctrl K</kbd>
				</button>
				<form id="signout" method="POST" action="/auth/logout" class="flex items-center gap-2">
					<span class="hidden text-sm text-ink-2 md:inline" title={data.user.email}>{data.user.name}</span>
					<button class="rounded-lg px-2 py-1.5 text-sm text-ink-2 hover:bg-surface-2" type="submit">Sign out</button>
				</form>
			</div>
		</div>
	</header>
	{#if data.devAuth}
		<div class="border-b border-line bg-warn-bg px-4 py-1.5 text-center text-xs text-warn" role="status">
			Development mode: sign-in is bypassed. Never expose this to a network.
		</div>
	{/if}
	{#if data.simulated.length}
		<div class="border-b border-line bg-warn-bg px-4 py-1.5 text-center text-xs text-warn" role="status">
			<strong>Simulated:</strong>
			{data.simulated.join(', ')}. Launching a business does not create anything in {data.simulated.length === 1 ? 'that system' : 'these systems'}.
		</div>
	{/if}
	<main id="main" class="mx-auto max-w-7xl px-4 py-6">
		{@render children()}
	</main>
	<CommandPalette {items} bind:open={paletteOpen} />
{:else}
	<main class="mx-auto max-w-md px-4 py-20">{@render children()}</main>
{/if}
