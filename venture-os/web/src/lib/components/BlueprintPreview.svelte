<script lang="ts">
	import { accountGroups, orgTree, type Blueprint, type OrgNode } from '$lib/blueprint';
	import { formatBps, formatMoney, titleCase } from '$lib/format';

	let { blueprint }: { blueprint: Blueprint } = $props();

	const b = $derived(blueprint.business);
	const cur = $derived(b.base_currency);
	const org = $derived(orgTree(blueprint.agents ?? []));
	const groups = $derived(accountGroups(blueprint.accounting?.chart_of_accounts ?? []));
	const money = (m: number | undefined) => (m === undefined ? '—' : formatMoney(BigInt(Math.trunc(m)), cur));
</script>

{#snippet tree(nodes: OrgNode[])}
	<ul class="space-y-1.5 border-l border-line pl-3">
		{#each nodes as n (n.agent.key)}
			<li>
				<div class="rounded-lg bg-surface-2 px-3 py-2">
					<div class="flex flex-wrap items-baseline justify-between gap-x-2">
						<span class="text-sm font-medium">{n.agent.role_title}</span>
						<span class="text-xs text-ink-3">{n.agent.key}</span>
					</div>
					<p class="mt-0.5 text-xs text-ink-2">{n.agent.responsibilities.join(' · ')}</p>
				</div>
				{#if n.reports.length}<div class="mt-1.5">{@render tree(n.reports)}</div>{/if}
			</li>
		{/each}
	</ul>
{/snippet}

<div class="space-y-5 text-sm">
	<section>
		<h3 class="text-lg font-semibold tracking-tight">{b.name}</h3>
		<p class="text-ink-3">{b.slug} · {b.industry} · {b.country} · books in {cur}</p>
		{#if b.description}<p class="mt-2 text-ink-2">{b.description}</p>{/if}
		<dl class="mt-3 grid grid-cols-2 gap-3">
			<div><dt class="text-xs text-ink-3">Starting capital</dt><dd class="font-medium tabular">{money(b.starting_capital_minor)}</dd></div>
			<div><dt class="text-xs text-ink-3">Tax regime</dt><dd class="font-medium">{blueprint.accounting?.tax?.regime ?? '—'}</dd></div>
		</dl>
		{#if b.target_customers?.length}
			<p class="mt-3 text-xs text-ink-3">Customers</p>
			<div class="mt-1 flex flex-wrap gap-1.5">{#each b.target_customers as c (c)}<span class="rounded-full bg-surface-2 px-2 py-0.5 text-xs">{c}</span>{/each}</div>
		{/if}
		{#if b.offerings?.length}
			<p class="mt-3 text-xs text-ink-3">Offerings</p>
			<div class="mt-1 flex flex-wrap gap-1.5">{#each b.offerings as o (o.name)}<span class="rounded-full bg-surface-2 px-2 py-0.5 text-xs">{o.name} <span class="text-ink-3">· {titleCase(o.kind)}</span></span>{/each}</div>
		{/if}
	</section>

	{#if blueprint.pricing?.length}
		<section>
			<h4 class="mb-2 font-semibold">Pricing guardrails</h4>
			<ul class="space-y-2">
				{#each blueprint.pricing as p (p.name)}
					<li class="rounded-lg border border-line p-3">
						<div class="flex items-baseline justify-between"><span class="font-medium">{p.name}</span><span class="text-xs text-ink-3">{titleCase(p.model)} · {p.currency}</span></div>
						<p class="mt-1 text-ink-2">
							Never below <strong>{formatBps(p.margin_floor_bps)}</strong> margin{#if p.margin_target_bps !== undefined}, aiming for {formatBps(p.margin_target_bps)}{/if}.
							{#if p.approval_threshold_minor}Quotes of {formatMoney(BigInt(p.approval_threshold_minor), p.currency)} or more need your approval.{/if}
						</p>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if blueprint.crm?.pipelines?.length}
		<section>
			<h4 class="mb-2 font-semibold">CRM pipelines</h4>
			<div class="space-y-3">
				{#each blueprint.crm.pipelines as p (p.key)}
					<div>
						<p class="font-medium">{p.name} <span class="text-xs font-normal text-ink-3">· {titleCase(p.entity_type)}</span></p>
						<ol class="mt-1 flex flex-wrap items-center gap-1">
							{#each p.stages as s, i (s.key)}
								<li class="flex items-center gap-1">
									<span class="rounded-md bg-surface-2 px-2 py-0.5 text-xs">{s.name}</span>
									{#if i < p.stages.length - 1}<span class="text-ink-3" aria-hidden="true">›</span>{/if}
								</li>
							{/each}
						</ol>
					</div>
				{/each}
			</div>
		</section>
	{/if}

	{#if org.length}
		<section>
			<h4 class="mb-2 font-semibold">Your AI team <span class="font-normal text-ink-3">({blueprint.agents.length} agents)</span></h4>
			{@render tree(org)}
		</section>
	{/if}

	{#if blueprint.workflows?.length}
		<section>
			<h4 class="mb-2 font-semibold">Workflows</h4>
			<ul class="space-y-2">
				{#each blueprint.workflows as w (w.key)}
					<li class="rounded-lg border border-line p-3">
						<p class="font-medium">{w.name}{#if w.requires_approval}<span class="ml-2 rounded bg-warn-bg px-1.5 py-0.5 text-[11px] font-normal text-warn">ends in your approval</span>{/if}</p>
						<p class="mt-1 text-xs text-ink-2">{w.steps.map((s) => s.replace(/_/g, ' ')).join(' → ')}</p>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if blueprint.support?.channels?.length}
		<section>
			<h4 class="mb-2 font-semibold">Customer support</h4>
			<ul class="space-y-1">
				{#each blueprint.support.channels as c (c.name)}<li class="text-ink-2">{c.name} <span class="text-ink-3">· {titleCase(c.kind)}</span></li>{/each}
			</ul>
		</section>
	{/if}

	{#if groups.length}
		<details class="rounded-lg border border-line p-3">
			<summary class="cursor-pointer font-semibold">Chart of accounts <span class="font-normal text-ink-3">({blueprint.accounting.chart_of_accounts.length} accounts)</span></summary>
			<ul class="mt-2 space-y-1.5">
				{#each groups as g (g.account.code)}
					<li>
						<span class="tabular text-ink-3">{g.account.code}</span> <span class="font-medium">{g.account.name}</span>
						{#if g.children.length}
							<ul class="mt-0.5 ml-5 space-y-0.5 text-ink-2">
								{#each g.children as c (c.code)}<li><span class="tabular text-ink-3">{c.code}</span> {c.name}</li>{/each}
							</ul>
						{/if}
					</li>
				{/each}
			</ul>
		</details>
	{/if}
</div>
