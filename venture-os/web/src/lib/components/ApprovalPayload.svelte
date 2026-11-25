<script lang="ts">
	import type { PayloadView } from '$lib/approvals';
	import { formatBps } from '$lib/format';
	import Money from './Money.svelte';

	let { view, currency }: { view: PayloadView; currency: string } = $props();
	const toneClass = { bad: 'text-bad', warn: 'text-warn', good: 'text-good' } as const;
</script>

{#if view.flags.length}
	<div class="mb-4 rounded-lg border border-bad/30 bg-bad-bg p-3 text-sm text-bad" role="alert">
		<p class="font-semibold">Flagged by the pricing engine</p>
		<ul class="mt-1 list-disc pl-5">
			{#each view.flags as f (f)}<li>{f}</li>{/each}
		</ul>
	</div>
{/if}

{#if view.facts.length}
	<dl class="grid gap-x-6 gap-y-3 sm:grid-cols-2">
		{#each view.facts as f (f.label)}
			<div>
				<dt class="text-xs text-ink-3 capitalize">{f.label}</dt>
				<dd class="font-medium {f.tone ? toneClass[f.tone] : ''}">
					{#if f.money}<Money minor={f.money.minor} currency={f.money.currency} />{:else}{f.value || '—'}{/if}
				</dd>
			</div>
		{/each}
	</dl>
{/if}

{#if view.components?.length}
	<h3 class="mt-6 mb-2 text-sm font-semibold">Cost build-up</h3>
	<table class="w-full text-sm">
		<tbody>
			{#each view.components as c (c.name)}
				<tr class="border-b border-line last:border-0">
					<td class="py-1.5 capitalize text-ink-2">{c.name}</td>
					<td class="py-1.5 text-right"><Money minor={c.minor} {currency} /></td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

{#if view.lines?.length}
	<h3 class="mt-6 mb-2 text-sm font-semibold">Lines</h3>
	<div class="overflow-x-auto">
		<table class="w-full text-sm">
			<thead class="text-xs text-ink-3">
				<tr><th class="py-1 text-left font-medium">Description</th><th class="py-1 text-right font-medium">Qty</th><th class="py-1 text-right font-medium">Unit price</th><th class="py-1 text-right font-medium">Total</th></tr>
			</thead>
			<tbody>
				{#each view.lines as l, i (i)}
					<tr class="border-t border-line">
						<td class="py-1.5">{l.description}</td>
						<td class="py-1.5 text-right tabular">{l.quantity}</td>
						<td class="py-1.5 text-right"><Money minor={l.unitMinor} {currency} /></td>
						<td class="py-1.5 text-right"><Money minor={l.totalMinor} {currency} /></td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
	{#if view.totals}
		<dl class="mt-3 ml-auto max-w-xs space-y-1 text-sm">
			<div class="flex justify-between"><dt class="text-ink-3">Subtotal</dt><dd><Money minor={view.totals.subtotal} currency={view.totals.currency} /></dd></div>
			<div class="flex justify-between"><dt class="text-ink-3">Tax ({formatBps(view.totals.taxRateBps)})</dt><dd><Money minor={view.totals.tax} currency={view.totals.currency} /></dd></div>
			<div class="flex justify-between border-t border-line pt-1 text-base font-semibold"><dt>Total</dt><dd><Money minor={view.totals.total} currency={view.totals.currency} /></dd></div>
		</dl>
	{/if}
{/if}

{#if view.text}
	<h3 class="mt-6 mb-2 text-sm font-semibold">{view.kind === 'blocker' ? 'What the agent reports' : 'Note'}</h3>
	<p class="rounded-lg bg-surface-2 p-3 text-sm leading-relaxed whitespace-pre-wrap">{view.text}</p>
{/if}

{#if view.kind === 'generic' && !view.facts.length}
	<pre class="overflow-x-auto rounded-lg bg-surface-2 p-3 text-xs">{JSON.stringify(view.raw, (_, v) => (typeof v === 'bigint' ? v.toString() : v), 2)}</pre>
{/if}
