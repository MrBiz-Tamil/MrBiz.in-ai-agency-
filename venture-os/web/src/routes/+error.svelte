<script lang="ts">
	import { page } from '$app/state';
</script>

<div class="mx-auto max-w-lg py-16 text-center">
	<p class="text-sm font-medium text-ink-3">{page.status}</p>
	<h1 class="mt-2 text-2xl font-semibold tracking-tight">{page.status === 404 ? 'We could not find that' : 'Something is not right'}</h1>
	<p class="mt-3 text-ink-2">{page.error?.message ?? 'An unexpected error occurred.'}</p>
	<div class="mt-6 flex justify-center gap-3">
		<a href="/" class="rounded-lg bg-accent px-4 py-2 text-sm font-medium text-accent-ink">Back to portfolio</a>
		<button type="button" class="rounded-lg border border-line px-4 py-2 text-sm hover:bg-surface-2" onclick={() => location.reload()}>Try again</button>
	</div>
</div>
