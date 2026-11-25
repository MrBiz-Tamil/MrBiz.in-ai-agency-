<script lang="ts">
	// The outcome of the last form submission, shown where the user is looking. Failures use role="alert".
	let { form }: { form: { ok?: boolean; message?: string } | null | undefined } = $props();
</script>

{#if form?.ok === false && form.message}
	<p class="mb-4 rounded-lg bg-bad-bg px-3 py-2 text-sm text-bad" role="alert">{form.message}</p>
{/if}
