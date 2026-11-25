<script lang="ts">
	import { enhance } from '$app/forms';
	import Money from '$lib/components/Money.svelte';
	import Notice from '$lib/components/Notice.svelte';
	import { fieldHelp, help } from '$lib/integrations';
	import { minorDigits, minorToDecimal, timeAgo } from '$lib/format';

	let { data, form } = $props();
	const b = $derived(data.business);
	const connected = $derived(new Map(data.connections.map((c) => [c.provider, c])));
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
	const plain = (v: bigint | null, cur: string) => (v === null ? '' : minorToDecimal(v, minorDigits(cur)));
</script>

<svelte:head><title>Connections and controls · {b.name} · Venture OS</title></svelte:head>

<nav class="mb-4 text-sm"><a href="/b/{b.id}" class="text-ink-2 hover:underline">← {b.name}</a></nav>
<h1 class="text-2xl font-semibold tracking-tight">Connections and controls</h1>
<p class="mt-1 text-sm text-ink-2">Connect your meeting and notes tools, set hard limits on money going out, and check that the audit trail has not been altered.</p>

{#if data.restricted}
	<p class="mt-6 rounded-xl border border-line bg-surface p-5 text-sm text-ink-2" role="status">These settings are visible to approvers and founders.</p>
{:else}
	<div class="mt-4"><Notice {form} /></div>
	{#if form && 'connected' in form && form.connected}<p class="mb-4 rounded-lg bg-good-bg px-3 py-2 text-sm text-good" role="status">Connected {help(form.connected).name}. The credentials were checked with the provider and stored encrypted.</p>{/if}
	{#if form && 'disconnected' in form && form.disconnected}<p class="mb-4 rounded-lg bg-surface-2 px-3 py-2 text-sm text-ink-2" role="status">Disconnected {help(form.disconnected).name}. Its stored credentials were deleted.</p>{/if}
	{#if form && 'capSaved' in form && form.capSaved}<p class="mb-4 rounded-lg bg-good-bg px-3 py-2 text-sm text-good" role="status">Spend limits for {form.capSaved} saved.</p>{/if}
	{#if form && 'capRemoved' in form && form.capRemoved}<p class="mb-4 rounded-lg bg-surface-2 px-3 py-2 text-sm text-ink-2" role="status">Spend limits for {form.capRemoved} removed.</p>{/if}

	<section class="mt-6 space-y-4" aria-labelledby="conn-h">
		<h2 id="conn-h" class="text-lg font-semibold">Connections</h2>
		{#each data.providers as p (p.provider)}
			{@const h = help(p.provider)}
			{@const c = connected.get(p.provider)}
			<div class="rounded-xl border border-line bg-surface p-5">
				<div class="flex flex-wrap items-start justify-between gap-3">
					<div class="min-w-0">
						<h3 class="font-semibold">{h.name}</h3>
						<p class="text-sm text-ink-2">{h.summary}</p>
					</div>
					{#if c}<span class="rounded-full bg-good-bg px-2 py-0.5 text-xs font-medium text-good">Connected</span>{:else}<span class="rounded-full bg-surface-2 px-2 py-0.5 text-xs text-ink-2">Not connected</span>{/if}
				</div>
				<p class="mt-2 text-xs text-ink-3">{h.agents}</p>
				{#if c}
					<p class="mt-3 text-xs text-ink-3">Connected by {c.connectedBy} · {timeAgo(c.updatedAt)}{#each Object.entries(c.config) as [k, v] (k)} · {fieldHelp('settings', p.provider, k).label}: {v}{/each}</p>
				{/if}
				{#if data.canManage}
					<details class="mt-3">
						<summary class="cursor-pointer text-sm font-medium text-accent">{c ? 'Replace credentials' : 'Connect'}</summary>
						<form method="POST" action="?/connect" use:enhance class="mt-3 space-y-3" autocomplete="off">
							<input type="hidden" name="provider" value={p.provider} />
							{#each p.credentials as f (f)}
								{@const fh = fieldHelp('credentials', p.provider, f)}
								<label class="block text-sm"><span class="text-ink-2">{fh.label}</span>
									<input name="c_{f}" type={fh.secret ? 'password' : 'text'} required autocomplete="off" spellcheck="false" class={cls} />
									{#if fh.help}<span class="mt-1 block text-xs text-ink-3">{fh.help}</span>{/if}
								</label>
							{/each}
							{#each p.settings as f (f)}
								{@const fh = fieldHelp('settings', p.provider, f)}
								<label class="block text-sm"><span class="text-ink-2">{fh.label} <span class="text-ink-3">{p.provider === 'notion' && f === 'database_id' ? '' : '(optional)'}</span></span>
									<input name="s_{f}" value={c?.config[f] ?? ''} required={p.provider === 'notion' && f === 'database_id'} autocomplete="off" class={cls} />
									{#if fh.help}<span class="mt-1 block text-xs text-ink-3">{fh.help}</span>{/if}
								</label>
							{/each}
							<p class="text-xs text-ink-3">The credentials are checked with {h.name} first and only stored if they work. They are encrypted, never shown again, and never leave the server.</p>
							<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">{c ? 'Check and replace' : 'Check and connect'}</button>
						</form>
					</details>
					{#if c}
						<form method="POST" action="?/disconnect" use:enhance class="mt-2">
							<input type="hidden" name="provider" value={p.provider} />
							<button class="text-xs text-bad hover:underline">Disconnect and delete the stored credentials</button>
						</form>
					{/if}
				{:else if !c}
					<p class="mt-3 text-xs text-ink-3">Only a founder can connect accounts.</p>
				{/if}
			</div>
		{/each}
	</section>

	<section class="mt-8" aria-labelledby="caps-h">
		<h2 id="caps-h" class="text-lg font-semibold">Spend limits</h2>
		<p class="mt-1 text-sm text-ink-2">Hard ceilings on money going out (payments and purchase orders). When someone approves a request that would break a limit, it is refused, even a founder, until the limit is raised. Leave a box empty for no limit on that.</p>
		<div class="mt-3 rounded-xl border border-line bg-surface p-5">
			{#if data.caps.length === 0}
				<p class="text-sm text-ink-2">No limits are set, so nothing stops a large approval.</p>
			{:else}
				<ul class="divide-y divide-line">
					{#each data.caps as c (c.currency)}
						<li class="flex flex-wrap items-center justify-between gap-3 py-3 text-sm">
							<div class="font-medium">{c.currency}</div>
							<div class="flex flex-wrap gap-x-5 gap-y-1 text-ink-2">
								<span>Per payment: {#if c.perTransaction !== null}<Money minor={c.perTransaction} currency={c.currency} />{:else}no limit{/if}</span>
								<span>Per hour: {#if c.perHour !== null}<Money minor={c.perHour} currency={c.currency} />{:else}no limit{/if}</span>
								<span>Per day: {#if c.perDay !== null}<Money minor={c.perDay} currency={c.currency} />{:else}no limit{/if}</span>
							</div>
							{#if data.canManage}
								<form method="POST" action="?/removeCap" use:enhance><input type="hidden" name="currency" value={c.currency} /><button class="text-xs text-bad hover:underline">Remove</button></form>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
			{#if data.canManage}
				<form method="POST" action="?/setCap" use:enhance class="mt-4 grid gap-3 border-t border-line pt-4 sm:grid-cols-4">
					<label class="block text-sm"><span class="text-ink-2">Currency</span><input name="currency" value={b.currency} maxlength="3" required class="{cls} uppercase" /></label>
					<label class="block text-sm"><span class="text-ink-2">Per payment</span><input name="perTransaction" inputmode="decimal" placeholder="50,000" class={cls} /></label>
					<label class="block text-sm"><span class="text-ink-2">Per hour</span><input name="perHour" inputmode="decimal" placeholder="100,000" class={cls} /></label>
					<label class="block text-sm"><span class="text-ink-2">Per day</span><input name="perDay" inputmode="decimal" placeholder="500,000" class={cls} /></label>
					<p class="text-xs text-ink-3 sm:col-span-4">Saving replaces that currency's limits. A payment limit cannot exceed the hourly one, and the hourly one cannot exceed the daily one. Every change is recorded in the audit trail.</p>
					<div class="sm:col-span-4"><button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Save limits</button></div>
				</form>
			{/if}
		</div>
	</section>

	<section class="mt-8 mb-8" aria-labelledby="audit-h">
		<h2 id="audit-h" class="text-lg font-semibold">Audit trail</h2>
		<p class="mt-1 text-sm text-ink-2">Every action is recorded in a chain where each entry includes a fingerprint of the one before it. Checking walks the chain and reports the first entry that does not match, which would reveal an entry that was edited or removed from the middle.</p>
		<div class="mt-3 rounded-xl border border-line bg-surface p-5">
			<form method="POST" action="?/verifyAudit" use:enhance><button class="rounded-lg border border-line px-4 py-2 text-sm hover:bg-surface-2">Check the audit trail now</button></form>
			{#if form && 'audit' in form && form.audit}
				{#if form.audit.intact}
					<p class="mt-3 rounded-lg bg-good-bg px-3 py-2 text-sm text-good" role="status">Intact: {form.audit.rows} entries checked, none altered.</p>
				{:else}
					<p class="mt-3 rounded-lg bg-bad-bg px-3 py-2 text-sm text-bad" role="alert">The chain is broken at entry {form.audit.firstBadId}: that entry, or one before it, was changed or removed. Treat this as a security incident.</p>
				{/if}
				<p class="mt-3 text-xs text-ink-3">Latest fingerprint: <code class="font-mono break-all">{form.audit.head}</code></p>
				<p class="mt-1 text-xs text-ink-3">Keep a copy somewhere the database administrator cannot change. If the newest entries were ever deleted, the chain would still look intact on its own, but its latest fingerprint would no longer match your copy.</p>
			{/if}
		</div>
	</section>
{/if}
