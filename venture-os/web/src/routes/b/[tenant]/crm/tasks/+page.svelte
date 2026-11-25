<script lang="ts">
	import ActivityList from '$lib/components/ActivityList.svelte';
	import Notice from '$lib/components/Notice.svelte';

	let { data, form } = $props();
	const base = $derived(`/b/${data.business.id}/crm`);
</script>

<h2 class="text-lg font-semibold">Open tasks</h2>
<p class="mt-1 text-sm text-ink-2">Everything anyone, or any agent, has asked to be done and nobody has finished. Oldest first.</p>

<div class="mt-4"><Notice {form} /></div>

<section class="rounded-xl border border-line bg-surface p-5">
	<ActivityList
		activities={data.tasks}
		canWrite={data.canWrite}
		me={data.me}
		empty="No open tasks. Add one from a deal or a company."
		parentLink={(a) => (a.deal_id ? { href: `${base}/deals/${a.deal_id}`, label: 'Open deal' } : a.company_id ? { href: `${base}/companies/${a.company_id}`, label: 'Open company' } : null)}
	/>
	{#if data.next}<p class="mt-3 text-sm"><a class="text-accent hover:underline" href="?cursor={encodeURIComponent(data.next)}">Next 50 →</a></p>{/if}
</section>
