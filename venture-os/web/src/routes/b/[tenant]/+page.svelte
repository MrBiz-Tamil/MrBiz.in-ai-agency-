<script lang="ts">
	import Badge from '$lib/components/Badge.svelte';
	import Money from '$lib/components/Money.svelte';
	import ProvisionProgress from '$lib/components/ProvisionProgress.svelte';
	import { timeAgo } from '$lib/format';

	let { data } = $props();
	const b = $derived(data.business);
</script>

<svelte:head><title>{b.name} · Venture OS</title></svelte:head>

<nav class="mb-4 text-sm"><a href="/" class="text-ink-2 hover:underline">← Portfolio</a></nav>

<div class="flex flex-wrap items-center gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">{b.name}</h1>
	<Badge value={b.status} />
	<span class="text-sm text-ink-3">{b.slug} · you are {b.role}</span>
</div>

<div class="mt-6 grid gap-6 lg:grid-cols-[1fr_22rem]">
	<div class="space-y-6">
		{#if b.hasForecast}
			<section class="grid gap-3 sm:grid-cols-3" aria-label="Cash">
				<div class="rounded-xl border border-line bg-surface p-4">
					<div class="text-xs text-ink-3">Cash on hand</div>
					<div class="mt-1 text-2xl font-semibold"><Money minor={b.cash} currency={b.currency} /></div>
				</div>
				<div class="rounded-xl border border-line bg-surface p-4">
					<div class="text-xs text-ink-3">Lowest in the next 90 days</div>
					<div class="mt-1 text-2xl font-semibold {b.minCash < 0n ? 'text-bad' : ''}"><Money minor={b.minCash} currency={b.currency} /></div>
				</div>
				<div class="rounded-xl border border-line bg-surface p-4">
					<div class="text-xs text-ink-3">Runway</div>
					<div class="mt-1 text-2xl font-semibold {b.cashNegative ? 'text-bad' : 'text-good'}">{b.cashNegative ? (b.runwayDays ? `${b.runwayDays} days` : 'Negative') : '90+ days'}</div>
				</div>
			</section>
			<p class="-mt-3 text-xs text-ink-3">Based on the ledger and committed receipts and payments. Hoped-for income is not counted.</p>
		{/if}

		{#if b.status === 'ACTIVE'}
			<a href="/b/{b.id}/crm" class="flex items-center justify-between rounded-xl border border-line bg-surface p-5 hover:bg-surface-2">
				<div>
					<h2 class="font-semibold">Sales</h2>
					<p class="text-sm text-ink-2">Pipeline, companies and tasks.</p>
				</div>
				<span class="text-sm text-accent">Open →</span>
			</a>
		{/if}

		{#if b.status === 'ACTIVE' && b.role !== 'viewer'}
			<a href="/b/{b.id}/settings" class="flex items-center justify-between rounded-xl border border-line bg-surface p-5 hover:bg-surface-2">
				<div>
					<h2 class="font-semibold">Connections and controls</h2>
					<p class="text-sm text-ink-2">Google Calendar and Meet, Zoom, Notion, spend limits and the audit trail.</p>
				</div>
				<span class="text-sm text-accent">Open →</span>
			</a>
		{/if}

		<section class="rounded-xl border border-line bg-surface p-5">
			<div class="mb-3 flex items-center justify-between">
				<h2 class="font-semibold">Waiting for your approval</h2>
				{#if b.pending > 0}<a href="/approvals?b={b.id}" class="text-sm text-accent hover:underline">See all {b.pending} →</a>{/if}
			</div>
			{#if data.pending.length === 0}
				<p class="text-sm text-ink-2">{b.role === 'viewer' ? 'Approvals are visible to approvers and founders.' : 'Nothing is waiting on you.'}</p>
			{:else}
				<ul class="divide-y divide-line">
					{#each data.pending as a (a.id)}
						<li>
							<a href="/approvals/{a.tenantId}/{a.id}" class="flex items-center justify-between gap-3 py-2.5 hover:bg-surface-2">
								<div class="min-w-0">
									<p class="truncate text-sm font-medium">{a.title}</p>
									<p class="text-xs text-ink-3">{a.requestedBy} · {timeAgo(a.createdAt)}</p>
								</div>
								{#if a.amount !== null}<span class="shrink-0 text-sm font-semibold"><Money minor={a.amount} currency={a.currency} /></span>{/if}
							</a>
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	</div>

	<aside class="rounded-xl border border-line bg-surface p-5">
		<h2 class="mb-3 font-semibold">Setup</h2>
		{#if data.run}
			<div class="mb-3"><Badge value={data.run.status} /></div>
			<ProvisionProgress steps={data.run.steps} />
			{#if data.run.error}<p class="mt-3 text-xs text-bad">{data.run.error}</p>{/if}
		{:else}
			<p class="text-sm text-ink-2">This business has not been launched yet.</p>
			<a href="/launch?b={b.id}" class="mt-3 inline-block text-sm font-medium text-accent hover:underline">Continue setup →</a>
		{/if}
	</aside>
</div>
