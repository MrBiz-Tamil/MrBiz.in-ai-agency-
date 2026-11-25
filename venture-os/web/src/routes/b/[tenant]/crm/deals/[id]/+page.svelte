<script lang="ts">
	import { enhance } from '$app/forms';
	import ActivityForm from '$lib/components/ActivityForm.svelte';
	import ActivityList from '$lib/components/ActivityList.svelte';
	import Badge from '$lib/components/Badge.svelte';
	import CustomFields from '$lib/components/CustomFields.svelte';
	import Money from '$lib/components/Money.svelte';
	import Notice from '$lib/components/Notice.svelte';
	import { actorLabel } from '$lib/crm';
	import { formatDay, minorToDecimal, minorDigits, timeAgo } from '$lib/format';

	let { data, form } = $props();
	const d = $derived(data.deal);
	const base = $derived(`/b/${data.business.id}/crm`);
	const stageName = (key: string) => data.pipeline?.stages.find((s) => s.key === key)?.name ?? key;
	const amountText = $derived(d.amount ? minorToDecimal(BigInt(d.amount.amount_minor), minorDigits(d.amount.currency)) : '');
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
</script>

<svelte:head><title>{d.name} · CRM · Venture OS</title></svelte:head>

<nav class="mb-3 text-sm"><a href={base} class="text-ink-2 hover:underline">← Pipeline</a></nav>

<div class="flex flex-wrap items-center gap-3">
	<h2 class="text-xl font-semibold tracking-tight">{d.name}</h2>
	<Badge value={d.stage} label={stageName(d.stage)} tone="neutral" />
</div>
<p class="mt-1 text-sm text-ink-2">
	{#if data.company}<a href="{base}/companies/{data.company.id}" class="text-accent hover:underline">{data.company.name}</a> ·{/if}
	{data.pipeline?.name ?? d.pipeline}
	· in this stage since {timeAgo(d.stage_changed_at)}
</p>

<div class="mt-4"><Notice {form} /></div>
{#if form && 'saved' in form && form.saved}<p class="mb-4 rounded-lg bg-good-bg px-3 py-2 text-sm text-good" role="status">Saved.</p>{/if}

<div class="grid gap-6 lg:grid-cols-[1fr_22rem]">
	<div class="space-y-6">
		<section class="rounded-xl border border-line bg-surface p-5">
			<h3 class="mb-3 font-semibold">History</h3>
			{#if data.canWrite}
				<details class="mb-4 rounded-lg border border-line">
					<summary class="cursor-pointer px-3 py-2 text-sm font-medium">Add a note, call, meeting or task</summary>
					<div class="border-t border-line p-4"><ActivityForm idemKey={data.keys.activity} parent={{ name: 'deal_id', id: d.id }} /></div>
				</details>
			{/if}
			<ActivityList activities={data.activities} canWrite={data.canWrite} me={data.me} empty="No notes, calls or tasks on this deal yet." />
			{#if data.activities_truncated}<p class="mt-2 text-xs text-ink-3">Showing the first 50 entries.</p>{/if}
		</section>

		{#if data.stage_history && data.stage_history.length > 0}
			<section class="rounded-xl border border-line bg-surface p-5">
				<h3 class="mb-3 font-semibold">Stage changes</h3>
				<ol class="space-y-2 text-sm">
					{#each [...data.stage_history].reverse() as c, i (i)}
						<li class="flex flex-wrap items-baseline gap-x-2">
							<span class="font-medium">{c.from ? `${stageName(c.from)} → ${stageName(c.to)}` : `Created in ${stageName(c.to)}`}</span>
							<span class="text-xs text-ink-3">{c.changed_by ? `${actorLabel(c.changed_by, data.me)} · ` : ''}{timeAgo(c.changed_at)}</span>
						</li>
					{/each}
				</ol>
			</section>
		{/if}
	</div>

	<aside class="space-y-6">
		<section class="rounded-xl border border-line bg-surface p-5">
			<h3 class="mb-3 font-semibold">Details</h3>
			<dl class="space-y-2 text-sm">
				<div class="flex justify-between gap-3"><dt class="text-ink-3">Value</dt><dd class="font-semibold">{#if d.amount}<Money minor={BigInt(d.amount.amount_minor)} currency={d.amount.currency} />{:else}<span class="font-normal text-ink-3">Not set</span>{/if}</dd></div>
				<div class="flex justify-between gap-3"><dt class="text-ink-3">Expected close</dt><dd>{d.expected_close ? formatDay(d.expected_close) : '—'}</dd></div>
				{#each data.pipeline?.custom_fields ?? [] as f (f.key)}
					<div class="flex justify-between gap-3"><dt class="text-ink-3">{f.label}</dt><dd class="text-right">{d.fields?.[f.key] ?? '—'}</dd></div>
				{/each}
			</dl>
			<p class="mt-3 text-xs text-ink-3">The value is a pipeline estimate, not a quote.</p>
		</section>

		{#if data.canWrite && data.pipeline}
			<section class="rounded-xl border border-line bg-surface p-5">
				<h3 class="mb-3 font-semibold">Move to stage</h3>
				<form method="POST" action="?/move" use:enhance class="flex gap-2">
					<input type="hidden" name="deal_id" value={d.id} />
					<input type="hidden" name="version" value={d.version ?? ''} />
					<select name="stage" aria-label="Stage" class="min-w-0 flex-1 rounded-lg border border-line bg-surface px-3 py-2 text-sm">
						{#each data.pipeline.stages as s (s.key)}<option value={s.key} selected={s.key === d.stage}>{s.name}</option>{/each}
					</select>
					<button class="rounded-lg bg-accent px-3 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Move</button>
				</form>
			</section>

			<section class="rounded-xl border border-line bg-surface p-5">
				<details>
					<summary class="cursor-pointer font-semibold">Edit details</summary>
					<form method="POST" action="?/update" use:enhance class="mt-4 space-y-3">
						<input type="hidden" name="version" value={d.version ?? ''} />
						<label class="block text-sm"><span class="text-ink-2">Name</span><input name="name" value={d.name} required maxlength="200" class={cls} /></label>
						<div class="grid grid-cols-[1fr_6rem] gap-2">
							<label class="block text-sm"><span class="text-ink-2">Estimated value</span><input name="amount" inputmode="decimal" value={amountText} placeholder="48,000.00" class={cls} /></label>
							<label class="block text-sm"><span class="text-ink-2">Currency</span><input name="currency" value={d.amount?.currency ?? data.business.currency} maxlength="3" class="{cls} uppercase" /></label>
						</div>
						<label class="block text-sm"><span class="text-ink-2">Expected close</span><input type="date" name="expected_close" value={d.expected_close ?? ''} class={cls} /></label>
						<CustomFields defs={data.pipeline.custom_fields} values={d.fields ?? {}} />
						<p class="text-xs text-ink-3">Clearing a value removes it. If someone else changed this deal meanwhile, you will be asked to reload instead of overwriting their change.</p>
						<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Save changes</button>
					</form>
				</details>
			</section>
			<section class="rounded-xl border border-line bg-surface p-5">
				<details>
					<summary class="cursor-pointer font-semibold text-bad">Archive deal…</summary>
					<form method="POST" action="?/archive" class="mt-3 space-y-2">
						<input type="hidden" name="version" value={d.version ?? ''} />
						<p class="text-sm text-ink-2">The deal leaves the pipeline and searches; its history is kept.</p>
						<button class="rounded-lg border border-bad px-4 py-2 text-sm text-bad hover:bg-bad-bg">Yes, archive this deal</button>
					</form>
				</details>
			</section>
		{/if}
	</aside>
</div>
