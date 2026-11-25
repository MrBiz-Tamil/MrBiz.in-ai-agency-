<script lang="ts">
	// Records a note, call, email, meeting or task on a deal or company. "Email" records one; it does not send it.
	import { enhance } from '$app/forms';
	import { ACTIVITY_KINDS } from '$lib/crm';
	import { titleCase } from '$lib/format';

	let { idemKey, parent }: { idemKey: string; parent: { name: 'deal_id' | 'company_id'; id: string } } = $props();
	let kind = $state('NOTE');
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
</script>

<form method="POST" action="?/activity" use:enhance class="space-y-3">
	<input type="hidden" name="key" value={idemKey} />
	<input type="hidden" name={parent.name} value={parent.id} />
	<div class="grid gap-3 sm:grid-cols-[10rem_1fr]">
		<label class="block text-sm"><span class="text-ink-2">Type</span>
			<select name="kind" bind:value={kind} class={cls}>
				{#each ACTIVITY_KINDS as k (k)}<option value={k}>{titleCase(k)}</option>{/each}
			</select>
		</label>
		<label class="block text-sm"><span class="text-ink-2">Subject</span>
			<input name="subject" required maxlength="200" class={cls} placeholder={kind === 'TASK' ? 'What needs doing?' : 'What happened?'} />
		</label>
	</div>
	<label class="block text-sm"><span class="text-ink-2">Details <span class="text-ink-3">(optional)</span></span>
		<textarea name="body" rows="3" maxlength="20000" class={cls}></textarea>
	</label>
	{#if kind === 'TASK'}
		<label class="block text-sm sm:w-56"><span class="text-ink-2">Due <span class="text-ink-3">(optional)</span></span>
			<input type="date" name="due" class={cls} />
		</label>
	{/if}
	{#if kind === 'EMAIL'}
		<p class="text-xs text-ink-3">This records an email in the history. It does not send anything.</p>
	{/if}
	<button class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink hover:opacity-90">Add to history</button>
</form>
