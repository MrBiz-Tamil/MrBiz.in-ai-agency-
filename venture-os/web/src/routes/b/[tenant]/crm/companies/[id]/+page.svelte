<script lang="ts">
	import { enhance } from '$app/forms';
	import ActivityForm from '$lib/components/ActivityForm.svelte';
	import ActivityList from '$lib/components/ActivityList.svelte';
	import Money from '$lib/components/Money.svelte';
	import Notice from '$lib/components/Notice.svelte';
	import { columnTotals } from '$lib/crm';
	import { formatDay } from '$lib/format';

	let { data, form } = $props();
	const c = $derived(data.company);
	const base = $derived(`/b/${data.business.id}/crm`);
	const stageName = (d: { pipeline: string; stage: string }) =>
		data.pipelines.find((p) => p.key === d.pipeline)?.stages.find((s) => s.key === d.stage)?.name ?? d.stage;
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
	const fullName = (k: { first_name?: string; last_name?: string; email?: string }) => [k.first_name, k.last_name].filter(Boolean).join(' ') || k.email || 'Unnamed contact';
</script>

<svelte:head><title>{c.name} · CRM · Venture OS</title></svelte:head>

<nav class="mb-3 text-sm"><a href="{base}/companies" class="text-ink-2 hover:underline">← Companies</a></nav>
<h2 class="text-xl font-semibold tracking-tight">{c.name}</h2>
<p class="mt-1 text-sm text-ink-2">{[c.domain, c.country, c.industry, c.phone].filter(Boolean).join(' · ') || 'No further details'}</p>

<div class="mt-4"><Notice {form} /></div>
{#if form && 'saved' in form && form.saved}<p class="mb-4 rounded-lg bg-good-bg px-3 py-2 text-sm text-good" role="status">Saved.</p>{/if}

<div class="grid gap-6 lg:grid-cols-[1fr_22rem]">
	<div class="space-y-6">
		<section class="rounded-xl border border-line bg-surface p-5">
			<div class="mb-3 flex items-center justify-between">
				<h3 class="font-semibold">Deals</h3>
				{#if data.canWrite}<a href="{base}?new={c.id}" class="text-sm text-accent hover:underline">New deal for {c.name} →</a>{/if}
			</div>
			{#if data.deals.length === 0}
				<p class="text-sm text-ink-2">No deals with this company yet.</p>
			{:else}
				<ul class="divide-y divide-line">
					{#each data.deals as d (d.id)}
						<li>
							<a href="{base}/deals/{d.id}" class="flex items-center justify-between gap-3 py-2.5 hover:bg-surface-2">
								<div class="min-w-0">
									<p class="truncate text-sm font-medium">{d.name}</p>
									<p class="text-xs text-ink-3">{stageName(d)}{#if d.expected_close} · close {formatDay(d.expected_close)}{/if}</p>
								</div>
								{#if d.amount}<span class="shrink-0 text-sm font-semibold"><Money minor={BigInt(d.amount.amount_minor)} currency={d.amount.currency} /></span>{/if}
							</a>
						</li>
					{/each}
				</ul>
				{#if columnTotals(data.deals).length}<p class="mt-2 flex flex-wrap gap-x-3 text-xs text-ink-3"><span>Estimated total:</span>{#each columnTotals(data.deals) as t (t.currency)}<Money minor={t.minor} currency={t.currency} />{/each}</p>{/if}
				{#if data.deals_truncated}<p class="mt-1 text-xs text-ink-3">Showing the first 50 deals.</p>{/if}
			{/if}
		</section>

		<section class="rounded-xl border border-line bg-surface p-5">
			<h3 class="mb-3 font-semibold">Open tasks</h3>
			<ActivityList activities={data.open_tasks} canWrite={data.canWrite} me={data.me} empty="Nothing to do for this company." />
			{#if data.canWrite}
				<details class="mt-4 rounded-lg border border-line">
					<summary class="cursor-pointer px-3 py-2 text-sm font-medium">Add a note, call, meeting or task</summary>
					<div class="border-t border-line p-4"><ActivityForm idemKey={data.keys.activity} parent={{ name: 'company_id', id: c.id }} /></div>
				</details>
			{/if}
		</section>
	</div>

	<aside class="space-y-6">
		<section class="rounded-xl border border-line bg-surface p-5">
			<h3 class="mb-3 font-semibold">Contacts</h3>
			{#if data.contacts.length === 0}
				<p class="text-sm text-ink-2">No contacts yet.</p>
			{:else}
				<ul class="space-y-2.5 text-sm">
					{#each data.contacts as k (k.id)}
						<li>
							<p class="font-medium">{fullName(k)}{#if k.title}<span class="font-normal text-ink-3"> · {k.title}</span>{/if}</p>
							<p class="text-xs text-ink-3">{[k.email, k.phone].filter(Boolean).join(' · ')}</p>
							{#if data.canWrite}
								<details class="mt-1">
									<summary class="cursor-pointer text-xs text-accent">Edit or archive</summary>
									<form method="POST" action="?/updateContact" use:enhance class="mt-2 space-y-2">
										<input type="hidden" name="contact_id" value={k.id} />
										<input type="hidden" name="version" value={k.version ?? ''} />
										<div class="grid grid-cols-2 gap-2">
											<label class="block text-xs"><span class="text-ink-2">First name</span><input name="first_name" value={k.first_name ?? ''} maxlength="100" class={cls} /></label>
											<label class="block text-xs"><span class="text-ink-2">Last name</span><input name="last_name" value={k.last_name ?? ''} maxlength="100" class={cls} /></label>
										</div>
										<label class="block text-xs"><span class="text-ink-2">Email</span><input type="email" name="email" value={k.email ?? ''} class={cls} /></label>
										<label class="block text-xs"><span class="text-ink-2">Job title</span><input name="title" value={k.title ?? ''} maxlength="100" class={cls} /></label>
										<label class="block text-xs"><span class="text-ink-2">Phone</span><input name="phone" value={k.phone ?? ''} maxlength="50" class={cls} /></label>
										<button class="rounded-lg bg-accent px-3 py-1.5 text-xs font-medium text-accent-ink hover:opacity-90">Save contact</button>
									</form>
									<form method="POST" action="?/archiveContact" use:enhance class="mt-3 rounded-lg border border-line p-2">
										<input type="hidden" name="contact_id" value={k.id} />
										<input type="hidden" name="version" value={k.version ?? ''} />
										<p class="text-xs text-ink-2">Archiving hides {fullName(k)} from lists. Their history is kept.</p>
										<button class="mt-1.5 rounded-lg border border-bad px-3 py-1.5 text-xs text-bad hover:bg-bad-bg">Yes, archive this contact</button>
									</form>
								</details>
							{/if}
						</li>
					{/each}
				</ul>
				{#if data.contacts_truncated}<p class="mt-2 text-xs text-ink-3">Showing the first 50 contacts.</p>{/if}
			{/if}
			{#if data.canWrite}
				<details class="mt-4">
					<summary class="cursor-pointer text-sm font-medium text-accent">Add a contact</summary>
					<form method="POST" action="?/contact" use:enhance class="mt-3 space-y-3">
						<input type="hidden" name="key" value={data.keys.contact} />
						<div class="grid grid-cols-2 gap-2">
							<label class="block text-sm"><span class="text-ink-2">First name</span><input name="first_name" maxlength="100" class={cls} /></label>
							<label class="block text-sm"><span class="text-ink-2">Last name</span><input name="last_name" maxlength="100" class={cls} /></label>
						</div>
						<label class="block text-sm"><span class="text-ink-2">Email</span><input type="email" name="email" class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Job title <span class="text-ink-3">(optional)</span></span><input name="title" maxlength="100" class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Phone <span class="text-ink-3">(optional)</span></span><input name="phone" maxlength="50" class={cls} /></label>
						<p class="text-xs text-ink-3">A name or an email is needed.</p>
						<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Add contact</button>
					</form>
				</details>
			{/if}
		</section>
		{#if data.canWrite}
			<section class="rounded-xl border border-line bg-surface p-5">
				<details>
					<summary class="cursor-pointer font-semibold">Edit company</summary>
					<form method="POST" action="?/update" use:enhance class="mt-4 space-y-3">
						<input type="hidden" name="version" value={c.version ?? ''} />
						<label class="block text-sm"><span class="text-ink-2">Name</span><input name="name" value={c.name} required maxlength="200" class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Website domain</span><input name="domain" value={c.domain ?? ''} class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Country <span class="text-ink-3">(2-letter code)</span></span><input name="country" value={c.country ?? ''} maxlength="2" class="{cls} uppercase" /></label>
						<label class="block text-sm"><span class="text-ink-2">Industry</span><input name="industry" value={c.industry ?? ''} maxlength="100" class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Phone</span><input name="phone" value={c.phone ?? ''} maxlength="50" class={cls} /></label>
						<label class="block text-sm"><span class="text-ink-2">Website</span><input name="website" value={c.website ?? ''} maxlength="500" class={cls} /></label>
						<p class="text-xs text-ink-3">If someone else changed this company meanwhile, you will be asked to reload instead of overwriting their change.</p>
						<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Save changes</button>
					</form>
				</details>
			</section>
			<section class="rounded-xl border border-line bg-surface p-5">
				<details>
					<summary class="cursor-pointer font-semibold text-bad">Archive company…</summary>
					<form method="POST" action="?/archive" class="mt-3 space-y-2">
						<input type="hidden" name="version" value={c.version ?? ''} />
						<p class="text-sm text-ink-2">
							{c.name} disappears from lists and searches; its history is kept.
							{#if data.deals.length > 0}<strong class="text-warn">{data.deals.length === 1 ? 'Its 1 deal stays' : `Its ${data.deals.length} deals stay`} in the pipeline</strong> and must be archived separately.{/if}
						</p>
						<button class="rounded-lg border border-bad px-4 py-2 text-sm text-bad hover:bg-bad-bg">Yes, archive {c.name}</button>
					</form>
				</details>
			</section>
		{/if}
	</aside>
</div>
