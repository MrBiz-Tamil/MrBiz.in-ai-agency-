<script lang="ts">
	import { enhance } from '$app/forms';
	import Notice from '$lib/components/Notice.svelte';
	import { timeAgo } from '$lib/format';

	let { data, form } = $props();
	const base = $derived(`/b/${data.business.id}/crm/companies`);
	let adding = $state(false);
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
</script>

<div class="flex flex-wrap items-end justify-between gap-3">
	<h2 class="text-lg font-semibold">Companies</h2>
	<div class="flex flex-wrap items-center gap-2">
		<form method="GET" class="flex gap-2" role="search">
			<input name="q" value={data.q} placeholder="Search by name or domain" aria-label="Search companies" class="w-56 rounded-lg border border-line bg-surface px-3 py-1.5 text-sm" />
			<button class="rounded-lg border border-line px-3 py-1.5 text-sm hover:bg-surface-2">Search</button>
		</form>
		<a href="/b/{data.business.id}/crm/export?type=companies{data.q ? `&q=${encodeURIComponent(data.q)}` : ''}" class="rounded-lg border border-line px-3 py-1.5 text-sm hover:bg-surface-2" download>Export CSV</a>
		{#if data.canWrite}
			<button type="button" class="rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-accent-ink hover:opacity-90" onclick={() => (adding = !adding)} aria-expanded={adding}>New company</button>
		{/if}
	</div>
</div>

<div class="mt-4"><Notice {form} /></div>

{#if adding && data.canWrite}
	<form method="POST" action="?/create" use:enhance class="mb-5 space-y-3 rounded-xl border border-line bg-surface p-5">
		<h3 class="font-semibold">New company</h3>
		<p class="text-xs text-ink-3">Search first, so you do not add the same company twice.</p>
		<input type="hidden" name="key" value={data.keys.company} />
		<div class="grid gap-3 sm:grid-cols-2">
			<label class="block text-sm"><span class="text-ink-2">Name</span><input name="name" required maxlength="200" class={cls} /></label>
			<label class="block text-sm"><span class="text-ink-2">Website domain <span class="text-ink-3">(optional)</span></span><input name="domain" placeholder="acme.com" class={cls} /></label>
			<label class="block text-sm"><span class="text-ink-2">Country <span class="text-ink-3">(2-letter code, optional)</span></span><input name="country" maxlength="2" placeholder="DE" class="{cls} uppercase" /></label>
			<label class="block text-sm"><span class="text-ink-2">Industry <span class="text-ink-3">(optional)</span></span><input name="industry" maxlength="100" class={cls} /></label>
			<label class="block text-sm"><span class="text-ink-2">Phone <span class="text-ink-3">(optional)</span></span><input name="phone" maxlength="50" class={cls} /></label>
			<label class="block text-sm"><span class="text-ink-2">Website <span class="text-ink-3">(optional)</span></span><input name="website" maxlength="500" class={cls} /></label>
		</div>
		<div class="flex gap-2">
			<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Create company</button>
			<button type="button" class="rounded-lg px-4 py-2 text-sm text-ink-2 hover:bg-surface-2" onclick={() => (adding = false)}>Cancel</button>
		</div>
	</form>
{/if}

{#if data.companies.length === 0}
	<p class="rounded-xl border border-line bg-surface p-5 text-sm text-ink-2">{data.q ? `No company matches “${data.q}”.` : 'No companies yet.'}</p>
{:else}
	<ul class="divide-y divide-line rounded-xl border border-line bg-surface">
		{#each data.companies as c (c.id)}
			<li>
				<a href="{base}/{c.id}" class="flex items-center justify-between gap-3 px-4 py-3 hover:bg-surface-2">
					<div class="min-w-0">
						<p class="truncate text-sm font-medium">{c.name}</p>
						<p class="truncate text-xs text-ink-3">{[c.domain, c.country, c.industry].filter(Boolean).join(' · ') || 'No details'}</p>
					</div>
					<span class="shrink-0 text-xs text-ink-3">added {timeAgo(c.created_at)}</span>
				</a>
			</li>
		{/each}
	</ul>
	{#if data.next}
		<p class="mt-3 text-sm"><a class="text-accent hover:underline" href="{base}?{data.q ? `q=${encodeURIComponent(data.q)}&` : ''}cursor={encodeURIComponent(data.next)}">Next 50 →</a></p>
	{/if}
{/if}
