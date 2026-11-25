<script lang="ts">
	import { page } from '$app/stores';
	import { onMount } from 'svelte';
	import StatusPill from './StatusPill.svelte';
	import { LayoutDashboard, Users, LogOut } from 'lucide-svelte';
	import { probeBackend, type Health } from '$lib/health';

	interface Props {
		session: { email: string; displayName: string; role: string };
	}
	let { session }: Props = $props();

	let currentPath = $derived($page.url.pathname);

	// Real backend health instead of a hardcoded "HEALTHY".
	let health = $state<Health>('UNKNOWN');

	onMount(() => {
		let stopped = false;
		const check = async () => {
			const h = await probeBackend();
			if (!stopped) health = h;
		};
		check();
		const id = setInterval(check, 15000);
		return () => {
			stopped = true;
			clearInterval(id);
		};
	});
</script>

<header class="h-16 border-b border-surface-800 bg-surface-900/90 backdrop-blur-md px-6 flex items-center justify-between sticky top-0 z-40">
	<div class="flex items-center gap-8">
		<a href="/operations" class="flex items-center gap-3">
			<div class="w-8 h-8 rounded-lg bg-indigo-600 flex items-center justify-center font-bold text-white shadow-lg shadow-indigo-500/20">
				M
			</div>
			<div class="flex flex-col">
				<span class="font-bold text-sm tracking-wide text-slate-100">Mrbiz.in ABOS</span>
				<span class="text-[10px] text-slate-400 uppercase tracking-widest font-mono">Autonomous Brain</span>
			</div>
		</a>

		<nav class="flex items-center gap-1" aria-label="Main">
			<a
				href="/operations"
				aria-current={currentPath === '/operations' ? 'page' : undefined}
				class="flex items-center gap-2 px-3 py-1.5 rounded-md text-xs font-medium transition-colors {currentPath === '/operations' ? 'bg-surface-800 text-indigo-400' : 'text-slate-400 hover:text-slate-200 hover:bg-surface-800/50'}"
			>
				<LayoutDashboard class="w-4 h-4" />
				<span>Live Execution Board</span>
			</a>

			<a
				href="/agents"
				aria-current={currentPath === '/agents' ? 'page' : undefined}
				class="flex items-center gap-2 px-3 py-1.5 rounded-md text-xs font-medium transition-colors {currentPath === '/agents' ? 'bg-surface-800 text-indigo-400' : 'text-slate-400 hover:text-slate-200 hover:bg-surface-800/50'}"
			>
				<Users class="w-4 h-4" />
				<span>Agent Org Chart</span>
			</a>
		</nav>
	</div>

	<div class="flex items-center gap-4">
		<StatusPill status={health} label="BACKEND" />
		<div class="text-right leading-tight">
			<div class="text-xs text-slate-200">{session.displayName || session.email}</div>
			<div class="text-[10px] font-mono text-slate-500 uppercase">{session.role}</div>
		</div>
		<form method="POST" action="/logout">
			<button type="submit" class="p-2 rounded-lg bg-surface-800 hover:bg-surface-700 text-slate-300" title="Sign out" aria-label="Sign out">
				<LogOut class="w-4 h-4" />
			</button>
		</form>
	</div>
</header>
