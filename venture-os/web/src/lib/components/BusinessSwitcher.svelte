<script lang="ts">
	import { invalidateAll } from '$app/navigation';
	import { BIZ_COOKIE } from '$lib/constants';
	import type { BusinessView } from '$lib/types';

	let { businesses, current }: { businesses: BusinessView[]; current: string } = $props();

	let open = $state(false);
	let root: HTMLElement | undefined = $state();
	const selected = $derived(businesses.find((b) => b.id === current));

	async function pick(id: string) {
		// A UI preference only (which business the approvals inbox filters to); the server re-validates it per request.
		document.cookie = `${BIZ_COOKIE}=${id}; path=/; max-age=31536000; samesite=lax`;
		open = false;
		await invalidateAll();
	}

	function onWindow(e: MouseEvent | KeyboardEvent) {
		if (e instanceof KeyboardEvent) {
			if (e.key === 'Escape') open = false;
		} else if (root && !root.contains(e.target as Node)) {
			open = false;
		}
	}
</script>

<svelte:window onclick={onWindow} onkeydown={onWindow} />

<div class="relative" bind:this={root}>
	<button
		type="button"
		class="flex max-w-56 items-center gap-2 rounded-lg border border-line bg-surface px-3 py-1.5 text-sm hover:bg-surface-2"
		aria-haspopup="listbox"
		aria-expanded={open}
		onclick={() => (open = !open)}
	>
		<span class="truncate">{selected ? selected.name : 'All businesses'}</span>
		<svg class="size-3.5 shrink-0 text-ink-3" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true"><path d="M5.5 7.5 10 12l4.5-4.5z" /></svg>
	</button>
	{#if open}
		<ul class="absolute left-0 z-40 mt-1 max-h-80 w-64 overflow-auto rounded-xl border border-line bg-surface p-1 shadow-xl" role="listbox" aria-label="Choose business">
			<li role="option" aria-selected={current === 'all'}>
				<button type="button" class="flex w-full items-center justify-between rounded-lg px-3 py-2 text-left text-sm hover:bg-surface-2" onclick={() => pick('all')}>
					<span>All businesses</span>
					{#if current === 'all'}<span aria-hidden="true">✓</span>{/if}
				</button>
			</li>
			{#each businesses as b (b.id)}
				<li role="option" aria-selected={current === b.id}>
					<button type="button" class="flex w-full items-center justify-between gap-2 rounded-lg px-3 py-2 text-left text-sm hover:bg-surface-2" onclick={() => pick(b.id)}>
						<span class="truncate">{b.name}</span>
						{#if b.pending > 0}<span class="rounded-full bg-warn-bg px-1.5 text-xs text-warn">{b.pending}</span>{/if}
						{#if current === b.id}<span aria-hidden="true">✓</span>{/if}
					</button>
				</li>
			{/each}
		</ul>
	{/if}
</div>
