<script lang="ts">
	import { goto } from '$app/navigation';
	import { tick } from 'svelte';
	import { rank, type PaletteItem } from '$lib/palette';

	let { items, open = $bindable(false) }: { items: PaletteItem[]; open: boolean } = $props();

	let query = $state('');
	let active = $state(0);
	let input: HTMLInputElement | undefined = $state();
	let previouslyFocused: Element | null = null;

	const results = $derived(rank(items, query));
	// Keep the highlighted row valid as the result list changes.
	$effect(() => {
		if (active >= results.length) active = Math.max(0, results.length - 1);
	});

	$effect(() => {
		if (open) {
			previouslyFocused = document.activeElement;
			query = '';
			active = 0;
			tick().then(() => input?.focus());
		} else if (previouslyFocused instanceof HTMLElement) {
			previouslyFocused.focus();
			previouslyFocused = null;
		}
	});

	function onWindowKey(e: KeyboardEvent) {
		if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
			e.preventDefault();
			open = !open;
		}
	}

	function choose(item: PaletteItem | undefined) {
		if (!item) return;
		open = false;
		if (item.run) item.run();
		else if (item.href) goto(item.href);
	}

	function onKey(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.preventDefault();
			open = false;
		} else if (e.key === 'ArrowDown') {
			e.preventDefault();
			active = (active + 1) % Math.max(1, results.length);
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			active = (active - 1 + results.length) % Math.max(1, results.length);
		} else if (e.key === 'Enter') {
			e.preventDefault();
			choose(results[active]);
		}
	}

	$effect(() => {
		document.getElementById(`pal-opt-${active}`)?.scrollIntoView({ block: 'nearest' });
	});

	// Group headings appear when the group changes between consecutive results.
	const headed = $derived(results.map((r, i) => ({ r, heading: i === 0 || results[i - 1].group !== r.group ? r.group : null })));
</script>

<svelte:window onkeydown={onWindowKey} />

{#if open}
	<div class="fixed inset-0 z-50 flex items-start justify-center bg-black/40 p-4 pt-[12vh]" role="presentation" onmousedown={(e) => e.target === e.currentTarget && (open = false)}>
		<div class="w-full max-w-xl overflow-hidden rounded-xl border border-line bg-surface shadow-2xl" role="dialog" aria-modal="true" aria-label="Command palette">
			<div class="border-b border-line px-4 py-3">
				<input
					bind:this={input}
					bind:value={query}
					onkeydown={onKey}
					class="w-full bg-transparent text-base text-ink outline-none placeholder:text-ink-3"
					placeholder="Search businesses, approvals, actions…"
					role="combobox"
					aria-expanded="true"
					aria-controls="pal-list"
					aria-activedescendant={results.length ? `pal-opt-${active}` : undefined}
					autocomplete="off"
					spellcheck="false"
				/>
			</div>
			<ul id="pal-list" role="listbox" class="max-h-[50vh] overflow-y-auto p-2">
				{#each headed as { r, heading }, i (r.id)}
					{#if heading}
						<li role="presentation" class="px-2 pt-2 pb-1 text-[11px] font-semibold tracking-wide text-ink-3 uppercase">{heading}</li>
					{/if}
					<li
						id="pal-opt-{i}"
						role="option"
						aria-selected={i === active}
						class="flex cursor-pointer items-center justify-between gap-3 rounded-lg px-3 py-2 text-sm {i === active ? 'bg-accent text-accent-ink' : 'text-ink'}"
						onmousemove={() => (active = i)}
						onclick={() => choose(r)}
						onkeydown={null}
					>
						<span class="truncate">{r.label}</span>
						{#if r.hint}<span class="shrink-0 text-xs {i === active ? 'opacity-80' : 'text-ink-3'}">{r.hint}</span>{/if}
					</li>
				{:else}
					<li class="px-3 py-6 text-center text-sm text-ink-3">Nothing matches “{query}”.</li>
				{/each}
			</ul>
			<div class="flex gap-4 border-t border-line px-4 py-2 text-xs text-ink-3">
				<span><kbd class="font-mono">↑↓</kbd> navigate</span>
				<span><kbd class="font-mono">↵</kbd> open</span>
				<span><kbd class="font-mono">esc</kbd> close</span>
			</div>
		</div>
	</div>
{/if}
