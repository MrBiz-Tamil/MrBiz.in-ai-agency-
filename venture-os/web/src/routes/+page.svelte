<script lang="ts">
	import Badge from '$lib/components/Badge.svelte';
	import Money from '$lib/components/Money.svelte';
	import type { BusinessView } from '$lib/types';

	let { data } = $props();

	const active = $derived(data.businesses.filter((b) => b.status === 'ACTIVE'));
	const pending = $derived(data.businesses.reduce((n, b) => n + b.pending, 0));
	const high = $derived(data.businesses.reduce((n, b) => n + b.highRisk, 0));
	const attention = $derived(data.businesses.filter((b) => needsAttention(b)));

	function needsAttention(b: BusinessView): boolean {
		return b.cashNegative || ['FAILED', 'ROLLED_BACK', 'COMPENSATION_FAILED'].includes(b.lastRun) || b.status === 'SUSPENDED';
	}

	function runway(b: BusinessView): { text: string; tone: 'good' | 'bad' | 'neutral' } {
		if (!b.hasForecast) return { text: '', tone: 'neutral' };
		if (b.cashNegative) return { text: b.runwayDays ? `Cash runs out in ${b.runwayDays} day${b.runwayDays === 1 ? '' : 's'}` : 'Cash projected negative within 90 days', tone: 'bad' };
		return { text: 'Cash positive for the next 90 days', tone: 'good' };
	}
</script>

<div class="flex flex-wrap items-end justify-between gap-3">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Portfolio</h1>
		<p class="mt-1 text-sm text-ink-2">Every business you chair, in one place.</p>
	</div>
	<a href="/launch" class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Launch a new business</a>
</div>

<section class="mt-6 grid grid-cols-2 gap-3 md:grid-cols-4" aria-label="Summary">
	{#each [
		{ label: 'Active businesses', value: active.length, sub: `${data.businesses.length} in total`, tone: '' },
		{ label: 'Awaiting your approval', value: pending, sub: pending ? 'Across all businesses' : 'Nothing waiting', tone: pending ? 'text-warn' : '' },
		{ label: 'High-risk requests', value: high, sub: 'Payments and flagged quotes', tone: high ? 'text-bad' : '' },
		{ label: 'Need attention', value: attention.length, sub: attention.length ? 'Cash or setup problems' : 'All healthy', tone: attention.length ? 'text-bad' : 'text-good' }
	] as t (t.label)}
		<div class="rounded-xl border border-line bg-surface p-4">
			<div class="text-xs font-medium text-ink-3">{t.label}</div>
			<div class="mt-1 text-3xl font-semibold tabular {t.tone}">{t.value}</div>
			<div class="mt-0.5 text-xs text-ink-3">{t.sub}</div>
		</div>
	{/each}
</section>

{#if data.businesses.length === 0}
	<section class="mt-10 rounded-2xl border border-dashed border-line bg-surface p-10 text-center">
		<h2 class="text-lg font-semibold">Start your first business</h2>
		<p class="mx-auto mt-2 max-w-md text-sm text-ink-2">
			Describe your idea to the Venture Architect. It designs the operating model, sets up the books, CRM, support and an AI team, and asks for your approval before anything goes live.
		</p>
		<a href="/launch" class="mt-5 inline-block rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink">Explain your business</a>
	</section>
{:else}
	<section class="mt-6 grid gap-4 md:grid-cols-2 xl:grid-cols-3" aria-label="Businesses">
		{#each data.businesses as b (b.id)}
			{@const r = runway(b)}
			<article class="flex flex-col rounded-xl border border-line bg-surface p-5">
				<div class="flex items-start justify-between gap-3">
					<div class="min-w-0">
						<h2 class="truncate text-base font-semibold">
							<a class="hover:underline" href={b.status === 'DRAFT' ? `/launch?b=${b.id}` : `/b/${b.id}`}>{b.name}</a>
						</h2>
						<p class="truncate text-xs text-ink-3">{b.slug} · you are {b.role}</p>
					</div>
					<Badge value={b.status} />
				</div>

				{#if b.hasForecast}
					<dl class="mt-4 grid grid-cols-2 gap-3">
						<div>
							<dt class="text-xs text-ink-3">Cash on hand</dt>
							<dd class="text-lg font-semibold"><Money minor={b.cash} currency={b.currency} compact /></dd>
						</div>
						<div>
							<dt class="text-xs text-ink-3">Lowest in 90 days</dt>
							<dd class="text-lg font-semibold {b.minCash < 0n ? 'text-bad' : ''}"><Money minor={b.minCash} currency={b.currency} compact /></dd>
						</div>
					</dl>
					<p class="mt-3 text-sm {r.tone === 'bad' ? 'font-medium text-bad' : 'text-ink-2'}">{r.text}</p>
				{:else if b.status === 'DRAFT'}
					<p class="mt-4 text-sm text-ink-2">Setup is not finished. Pick up the conversation where you left off.</p>
				{/if}
				{#if ['FAILED', 'ROLLED_BACK', 'COMPENSATION_FAILED'].includes(b.lastRun)}
					<p class="mt-2 text-sm text-bad">The last launch attempt did not complete.</p>
				{/if}

				<div class="mt-auto flex items-center justify-between pt-4">
					{#if b.pending > 0}
						<a href="/approvals?b={b.id}" class="inline-flex items-center gap-2 rounded-lg bg-warn-bg px-3 py-1.5 text-sm font-medium text-warn hover:opacity-90">
							{b.pending} awaiting approval{#if b.highRisk > 0}<span class="rounded bg-bad-bg px-1.5 text-xs text-bad">{b.highRisk} high risk</span>{/if}
						</a>
					{:else if b.status === 'ACTIVE'}
						<span class="text-sm text-ink-3">Nothing waiting on you</span>
					{:else}
						<span></span>
					{/if}
					<a href={b.status === 'DRAFT' ? `/launch?b=${b.id}` : `/b/${b.id}`} class="text-sm font-medium text-accent hover:underline">
						{b.status === 'DRAFT' ? 'Continue setup' : 'Open'} →
					</a>
				</div>
			</article>
		{/each}
	</section>
{/if}
