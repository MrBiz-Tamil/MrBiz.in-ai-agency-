<script lang="ts">
	import { goto } from '$app/navigation';
	import Badge from '$lib/components/Badge.svelte';
	import Money from '$lib/components/Money.svelte';
	import { timeAgo, titleCase } from '$lib/format';
	import { RISK_ORDER } from '$lib/approvals';

	let { data } = $props();

	let kindFilter = $state('ALL');
	let selected = $state(0);

	const kinds = $derived(['ALL', ...new Set(data.approvals.map((a) => a.kind))]);
	// Highest risk first, then newest (the server returns newest first, and the sort is stable).
	const rows = $derived(
		data.approvals
			.filter((a) => kindFilter === 'ALL' || a.kind === kindFilter)
			.toSorted((a, b) => (RISK_ORDER[a.risk] ?? 9) - (RISK_ORDER[b.risk] ?? 9))
	);
	$effect(() => {
		if (selected >= rows.length) selected = Math.max(0, rows.length - 1);
	});

	const scopeName = $derived(data.scope === 'all' ? 'all businesses' : (data.approvals[0]?.business ?? 'this business'));
	const riskTone = { HIGH: 'bad', MEDIUM: 'warn', LOW: 'neutral' } as const;

	function onKey(e: KeyboardEvent) {
		const t = e.target as HTMLElement;
		if (t.closest('input, textarea, select, [contenteditable]') || e.metaKey || e.ctrlKey || e.altKey) return;
		if (e.key === 'j') selected = Math.min(rows.length - 1, selected + 1);
		else if (e.key === 'k') selected = Math.max(0, selected - 1);
		else if (e.key === 'Enter' && rows[selected]) goto(`/approvals/${rows[selected].tenantId}/${rows[selected].id}`);
		else return;
		e.preventDefault();
	}

	const href = (status: string) => `/approvals?status=${status}${data.scope !== 'all' ? `&b=${data.scope}` : ''}`;
</script>

<svelte:window onkeydown={onKey} />
<svelte:head><title>Approvals · Venture OS</title></svelte:head>

<div class="flex flex-wrap items-end justify-between gap-3">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Approval inbox</h1>
		<p class="mt-1 text-sm text-ink-2">Everything your AI teams want to do that needs a human, across {scopeName}.</p>
	</div>
	<p class="hidden text-xs text-ink-3 md:block"><kbd class="rounded border border-line px-1 font-mono">j</kbd> <kbd class="rounded border border-line px-1 font-mono">k</kbd> move · <kbd class="rounded border border-line px-1 font-mono">Enter</kbd> open</p>
</div>

<div class="mt-5 flex flex-wrap items-center gap-2" role="tablist" aria-label="Status">
	{#each data.statuses as s (s)}
		<a
			href={href(s)}
			role="tab"
			aria-selected={data.status === s}
			class="rounded-full px-3 py-1 text-sm {data.status === s ? 'bg-accent text-accent-ink' : 'border border-line bg-surface text-ink-2 hover:bg-surface-2'}"
		>
			{titleCase(s === 'PENDING_APPROVAL' ? 'Pending' : s)}
		</a>
	{/each}
	<span class="mx-1 h-5 w-px bg-line" aria-hidden="true"></span>
	{#each kinds as k (k)}
		<button
			type="button"
			class="rounded-full px-3 py-1 text-xs {kindFilter === k ? 'bg-surface-2 font-semibold text-ink ring-1 ring-line' : 'text-ink-3 hover:bg-surface-2'}"
			aria-pressed={kindFilter === k}
			onclick={() => (kindFilter = k)}
		>
			{k === 'ALL' ? 'All kinds' : titleCase(k)}
		</button>
	{/each}
</div>

{#if rows.length === 0}
	<section class="mt-8 rounded-2xl border border-dashed border-line bg-surface p-10 text-center">
		<h2 class="text-lg font-semibold">{data.status === 'PENDING_APPROVAL' ? 'Inbox zero' : 'Nothing here'}</h2>
		<p class="mt-1 text-sm text-ink-2">
			{data.status === 'PENDING_APPROVAL' ? 'No requests are waiting for you. Your agents will queue the next one here.' : 'No requests with this status.'}
		</p>
	</section>
{:else}
	<ul class="mt-4 divide-y divide-line overflow-hidden rounded-xl border border-line bg-surface" aria-label="Approvals">
		{#each rows as a, i (a.id)}
			<li class={i === selected ? 'bg-surface-2' : ''}>
				<a
					href="/approvals/{a.tenantId}/{a.id}"
					class="grid grid-cols-[1fr_auto] items-center gap-x-4 gap-y-1 px-4 py-3 hover:bg-surface-2"
					onmouseenter={() => (selected = i)}
					aria-current={i === selected ? 'true' : undefined}
				>
					<div class="min-w-0">
						<div class="flex flex-wrap items-center gap-2">
							<span class="truncate font-medium">{a.title}</span>
							<Badge value={a.risk} tone={riskTone[a.risk as keyof typeof riskTone] ?? 'neutral'} label="{titleCase(a.risk)} risk" />
							<span class="rounded bg-surface-2 px-1.5 py-0.5 text-[11px] text-ink-2">{titleCase(a.kind)}</span>
						</div>
						<p class="mt-0.5 truncate text-sm text-ink-2">{a.summary}</p>
						<p class="mt-0.5 text-xs text-ink-3">{a.business} · {a.requestedBy} · {timeAgo(a.createdAt)}</p>
					</div>
					<div class="text-right">
						{#if a.amount !== null}<div class="font-semibold"><Money minor={a.amount} currency={a.currency} /></div>{/if}
						{#if data.status !== 'PENDING_APPROVAL'}<Badge value={a.status} />{/if}
					</div>
				</a>
			</li>
		{/each}
	</ul>
{/if}
