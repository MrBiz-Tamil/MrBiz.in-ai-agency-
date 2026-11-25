<script lang="ts">
	import { enhance } from '$app/forms';
	import CustomFields from '$lib/components/CustomFields.svelte';
	import Money from '$lib/components/Money.svelte';
	import Notice from '$lib/components/Notice.svelte';
	import { columnTotals, groupByStage } from '$lib/crm';
	import { formatDay, formatBps } from '$lib/format';

	let { data, form } = $props();
	const base = $derived(`/b/${data.business.id}/crm`);
	const columns = $derived(data.pipeline ? groupByStage(data.pipeline, data.deals) : []);
	const companyList = $derived(Object.entries(data.companies).sort((a, b) => a[1].localeCompare(b[1])));
	let adding = $state(false);
	$effect(() => {
		if (data.newFor && data.canWrite) adding = true; // arrived from a company page
	});
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';

	// Drag a card onto a column to move it. This only drives the card's own stage selector, so a drag makes exactly the
	// same versioned request as choosing the stage from the list (which stays, for keyboards and no-JS browsers).
	let overStage = $state<string | null>(null);
	function drop(e: DragEvent, stage: string) {
		e.preventDefault();
		overStage = null;
		const id = e.dataTransfer?.getData('text/plain');
		const f = id ? (document.getElementById(`move-${id}`) as HTMLFormElement | null) : null;
		const sel = f?.querySelector('select');
		if (!f || !sel || sel.value === stage) return;
		sel.value = stage;
		f.requestSubmit();
	}
</script>

{#if !data.pipeline}
	<p class="rounded-xl border border-line bg-surface p-5 text-sm text-ink-2">This business has no sales pipelines yet.</p>
{:else}
	<div class="flex flex-wrap items-end justify-between gap-3">
		<div class="flex flex-wrap items-center gap-2">
			{#if data.pipelines.length > 1}
				<div class="flex gap-1" role="tablist" aria-label="Pipelines">
					{#each data.pipelines as p (p.key)}
						<a
							href="{base}?p={p.key}"
							role="tab"
							aria-selected={p.key === data.pipeline.key}
							class="rounded-lg px-3 py-1.5 text-sm {p.key === data.pipeline.key ? 'bg-accent text-accent-ink' : 'bg-surface-2 text-ink-2 hover:text-ink'}">{p.name}</a
						>
					{/each}
				</div>
			{:else}
				<h2 class="text-lg font-semibold">{data.pipeline.name}</h2>
			{/if}
		</div>
		<div class="flex flex-wrap items-center gap-2">
			<form method="GET" class="flex gap-2" role="search">
				<input type="hidden" name="p" value={data.pipeline.key} />
				<input name="q" value={data.q} placeholder="Search deals" aria-label="Search deals" class="w-44 rounded-lg border border-line bg-surface px-3 py-1.5 text-sm" />
				<button class="rounded-lg border border-line px-3 py-1.5 text-sm hover:bg-surface-2">Search</button>
			</form>
			<a href="{base}/export?type=deals&p={data.pipeline.key}{data.q ? `&q=${encodeURIComponent(data.q)}` : ''}" class="rounded-lg border border-line px-3 py-1.5 text-sm hover:bg-surface-2" download>Export CSV</a>
			{#if data.canWrite}
				<button type="button" class="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink hover:opacity-90" onclick={() => (adding = !adding)} aria-expanded={adding}>New deal</button>
			{/if}
		</div>
	</div>

	<div class="mt-4"><Notice {form} /></div>
	{#if data.q}<p class="mb-3 text-sm text-ink-2">Showing deals matching “{data.q}”. <a href="{base}?p={data.pipeline.key}" class="text-accent hover:underline">Clear</a></p>{/if}
	{#if data.truncated}<p class="mb-3 rounded-lg bg-warn-bg px-3 py-2 text-sm text-warn" role="status">This pipeline has more deals than fit on the board; showing the oldest 200. Use search to find the rest.</p>{/if}

	{#if adding && data.canWrite}
		<form method="POST" action="?/createDeal" use:enhance={() => async ({ result, update }) => { await update(); if (result.type === 'success') adding = false; }} class="mb-5 space-y-3 rounded-xl border border-line bg-surface p-5">
			<h3 class="font-semibold">New deal in {data.pipeline.name}</h3>
			<input type="hidden" name="key" value={data.keys.deal} />
			<input type="hidden" name="pipeline" value={data.pipeline.key} />
			<div class="grid gap-3 sm:grid-cols-2">
				<label class="block text-sm"><span class="text-ink-2">Deal name</span><input name="name" required maxlength="200" class={cls} /></label>
				<label class="block text-sm"><span class="text-ink-2">Company</span>
					<select name="company_id" required class={cls}>
						<option value="">Choose a company…</option>
						{#each companyList as [id, name] (id)}<option value={id} selected={id === data.newFor}>{name}</option>{/each}
					</select>
					<span class="mt-1 block text-xs text-ink-3">Not there? <a class="text-accent hover:underline" href="{base}/companies">Add the company first</a>.{#if data.companiesTruncated} Only the first 200 companies are listed.{/if}</span>
				</label>
				<label class="block text-sm"><span class="text-ink-2">Starting stage</span>
					<select name="stage" class={cls}>{#each data.pipeline.stages as s (s.key)}<option value={s.key}>{s.name}</option>{/each}</select>
				</label>
				<label class="block text-sm"><span class="text-ink-2">Expected close <span class="text-ink-3">(optional)</span></span><input type="date" name="expected_close" class={cls} /></label>
				<div class="grid grid-cols-[1fr_6rem] gap-2">
					<label class="block text-sm"><span class="text-ink-2">Estimated value <span class="text-ink-3">(optional)</span></span><input name="amount" inputmode="decimal" placeholder="48,000.00" class={cls} /></label>
					<label class="block text-sm"><span class="text-ink-2">Currency</span><input name="currency" value={data.business.currency} maxlength="3" class="{cls} uppercase" /></label>
				</div>
				<CustomFields defs={data.pipeline.custom_fields} />
			</div>
			<p class="text-xs text-ink-3">The value is your estimate for the pipeline, not a quote. Quotes are priced by the pricing engine and approved in the approval inbox.</p>
			<div class="flex gap-2">
				<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Create deal</button>
				<button type="button" class="rounded-lg px-4 py-2 text-sm text-ink-2 hover:bg-surface-2" onclick={() => (adding = false)}>Cancel</button>
			</div>
		</form>
	{/if}

	<div class="-mx-4 overflow-x-auto px-4 pb-3">
		<ol class="flex min-w-max gap-3" aria-label="{data.pipeline.name} stages">
			{#each columns as col (col.stage.key)}
				{@const totals = columnTotals(col.deals)}
				<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
				<li
					class="w-72 shrink-0 rounded-xl border bg-surface-2 p-3 {overStage === col.stage.key ? 'border-accent ring-2 ring-accent/40' : 'border-line'}"
					aria-label="{col.stage.name}, {col.deals.length} deals"
					ondragover={(e) => { if (data.canWrite && !col.orphan) { e.preventDefault(); overStage = col.stage.key; } }}
					ondragleave={() => { if (overStage === col.stage.key) overStage = null; }}
					ondrop={(e) => { if (data.canWrite && !col.orphan) drop(e, col.stage.key); }}
				>
					<div class="mb-2 flex items-baseline justify-between gap-2">
						<h3 class="text-sm font-semibold">{col.stage.name}</h3>
						<span class="text-xs text-ink-3">{col.orphan ? col.deals.length : `${col.deals.length} · ${formatBps(col.stage.win_probability_bps)}`}</span>
					</div>
					{#if totals.length}
						<p class="mb-2 flex flex-wrap gap-x-3 text-xs text-ink-2">{#each totals as t (t.currency)}<Money minor={t.minor} currency={t.currency} compact />{/each}</p>
					{/if}
					<ul class="space-y-2">
						{#each col.deals as d (d.id)}
							<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
							<li
								class="rounded-lg border border-line bg-surface p-3 shadow-sm {data.canWrite ? 'cursor-grab active:cursor-grabbing' : ''}"
								draggable={data.canWrite}
								ondragstart={(e) => { e.dataTransfer?.setData('text/plain', d.id); if (e.dataTransfer) e.dataTransfer.effectAllowed = 'move'; }}
							>
								<a href="{base}/deals/{d.id}" class="text-sm font-medium hover:underline">{d.name}</a>
								<p class="mt-0.5 text-xs text-ink-3">{data.companies[d.company_id] ?? (data.companiesTruncated ? 'Company' : 'Archived company')}</p>
								<div class="mt-2 flex items-center justify-between gap-2 text-xs">
									{#if d.amount}<span class="font-semibold"><Money minor={BigInt(d.amount.amount_minor)} currency={d.amount.currency} /></span>{:else}<span class="text-ink-3">No value</span>{/if}
									{#if d.expected_close}<span class="text-ink-3">Close {formatDay(d.expected_close)}</span>{/if}
								</div>
								{#if data.canWrite && data.pipeline}
									<form id="move-{d.id}" method="POST" action="?/move" use:enhance class="mt-2">
										<input type="hidden" name="deal_id" value={d.id} />
										<input type="hidden" name="version" value={d.version ?? ''} />
										<select name="stage" aria-label="Move {d.name} to stage" class="w-full rounded-md border border-line bg-surface px-2 py-1 text-xs" onchange={(e) => e.currentTarget.form?.requestSubmit()}>
											{#each data.pipeline.stages as s (s.key)}<option value={s.key} selected={s.key === d.stage}>{s.key === d.stage ? s.name : `Move to ${s.name}`}</option>{/each}
											{#if col.orphan}<option value="" selected disabled>Choose a stage…</option>{/if}
										</select>
										<noscript><button class="mt-1 rounded border border-line px-2 py-0.5 text-xs">Move</button></noscript>
									</form>
								{/if}
							</li>
						{:else}
							<li class="rounded-lg border border-dashed border-line px-3 py-4 text-center text-xs text-ink-3">No deals</li>
						{/each}
					</ul>
				</li>
			{/each}
		</ol>
	</div>
{/if}
