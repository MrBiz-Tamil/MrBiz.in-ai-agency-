<script lang="ts">
	import { enhance } from '$app/forms';

	let { data, form } = $props();
	let submitting = $state(false);
</script>

<svelte:head><title>Sign in | ABOS</title></svelte:head>

<div class="flex-1 flex items-center justify-center p-6">
	<form
		method="POST"
		class="w-full max-w-sm bg-surface-900 border border-surface-800 rounded-2xl p-6 space-y-4"
		use:enhance={() => {
			submitting = true;
			return async ({ update }) => {
				await update({ reset: false });
				submitting = false;
			};
		}}
	>
		<div class="flex items-center gap-3">
			<div class="w-9 h-9 rounded-lg bg-indigo-600 flex items-center justify-center font-bold text-white">M</div>
			<div>
				<h1 class="text-sm font-bold text-slate-100">Mrbiz.in ABOS</h1>
				<p class="text-[11px] text-slate-400 font-mono">Sign in to continue</p>
			</div>
		</div>

		<input type="hidden" name="next" value={data.next} />

		<div>
			<label for="email" class="block text-[10px] uppercase font-mono text-slate-400 mb-1">Email</label>
			<input id="email" name="email" type="email" autocomplete="username" required value={form?.email ?? ''}
				class="w-full bg-surface-950 border border-surface-800 rounded-lg px-3 py-2 text-sm text-slate-200 focus:outline-none focus:border-indigo-500" />
		</div>
		<div>
			<label for="password" class="block text-[10px] uppercase font-mono text-slate-400 mb-1">Password</label>
			<input id="password" name="password" type="password" autocomplete="current-password" required
				class="w-full bg-surface-950 border border-surface-800 rounded-lg px-3 py-2 text-sm text-slate-200 focus:outline-none focus:border-indigo-500" />
		</div>

		{#if form?.error}
			<p class="text-xs text-rose-400" role="alert">{form.error}</p>
		{/if}

		<button type="submit" disabled={submitting}
			class="w-full py-2.5 bg-indigo-600 hover:bg-indigo-500 disabled:opacity-60 text-white font-semibold text-xs rounded-lg transition-colors">
			{submitting ? 'Signing in…' : 'Sign in'}
		</button>
	</form>
</div>
