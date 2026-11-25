<script lang="ts">
	import { PhoneCall, PhoneOff, Mic, MicOff, UserCheck, Clock } from 'lucide-svelte';
	import { onDestroy } from 'svelte';

	// DEMO ONLY: no LiveKit / audio connection exists yet; this simulates the call UI.
	let callStatus = $state<'IDLE' | 'CONNECTING' | 'LIVE' | 'TRANSFERRING'>('IDLE');
	let isMuted = $state(false);
	let elapsed = $state(0); // seconds since the (simulated) call went live

	let connectTimer: ReturnType<typeof setTimeout> | undefined;
	let transferTimer: ReturnType<typeof setTimeout> | undefined;
	let tick: ReturnType<typeof setInterval> | undefined;

	function clearTimers() {
		clearTimeout(connectTimer);
		clearTimeout(transferTimer);
		clearInterval(tick);
		tick = undefined;
	}

	function startCall() {
		if (callStatus !== 'IDLE') return;
		callStatus = 'CONNECTING';
		elapsed = 0;
		isMuted = false;
		connectTimer = setTimeout(() => {
			callStatus = 'LIVE';
			tick = setInterval(() => (elapsed += 1), 1000);
		}, 600);
	}

	function endCall() {
		clearTimers();
		callStatus = 'IDLE';
		elapsed = 0;
	}

	function transferToHuman() {
		if (callStatus !== 'LIVE') return;
		clearInterval(tick);
		tick = undefined;
		callStatus = 'TRANSFERRING';
		transferTimer = setTimeout(endCall, 1500);
	}

	onDestroy(clearTimers);
</script>

<div class="fixed bottom-6 right-6 z-50">
	{#if callStatus === 'IDLE'}
		<!-- Minimized Call Launcher Button -->
		<button 
			type="button" 
			class="flex items-center gap-2.5 px-4 py-3 rounded-full bg-indigo-600 hover:bg-indigo-500 text-white font-semibold text-xs shadow-xl shadow-indigo-600/30 transition-all hover:scale-105"
			onclick={startCall}
		>
			<PhoneCall class="w-4 h-4 animate-bounce" />
			<span>Voice agent demo (simulated)</span>
		</button>
	{:else}
		<!-- Active Call Panel -->
		<div class="w-96 bg-surface-900 border border-surface-800 rounded-2xl p-5 shadow-2xl flex flex-col">
			<!-- Header -->
			<div class="flex justify-between items-center pb-3 border-b border-surface-800">
				<div class="flex items-center gap-2">
					<span class="w-2.5 h-2.5 rounded-full {callStatus === 'LIVE' ? 'bg-emerald-400 animate-ping' : 'bg-amber-400'}"></span>
					<span class="text-xs font-bold text-slate-100 font-mono">
						{callStatus === 'LIVE' ? `CALL ACTIVE (${elapsed}s)` : callStatus}
					</span>
				</div>
				<div class="flex items-center gap-1.5 px-2 py-0.5 rounded bg-surface-950 border border-surface-800 text-[10px] font-mono text-amber-400">
					<Clock class="w-3 h-3" />
					<span>SIMULATED, no live audio</span>
				</div>
			</div>

			<!-- Audio Visualizer Waveform -->
			<div class="my-5 flex items-center justify-center gap-1 h-12 bg-surface-950 rounded-xl px-4 border border-surface-800">
				{#each [18, 36, 14, 42, 28, 16, 38, 22, 44, 30, 18, 35] as height, idx}
					<div 
						class="w-1.5 bg-indigo-500 rounded-full transition-all duration-200"
						style="height: {callStatus === 'LIVE' ? height : 6}px; opacity: {isMuted ? 0.3 : 1};"
					></div>
				{/each}
			</div>

			<!-- Live Transcript Box -->
			<div class="space-y-2 text-xs mb-5 bg-surface-950 p-3 rounded-xl border border-surface-800 max-h-36 overflow-y-auto">
				<div class="text-slate-400">
					<span class="font-semibold text-slate-300">Customer:</span> Where is my order #1042?
				</div>
				<div class="text-indigo-300">
					<span class="font-semibold text-indigo-400">Talker:</span> Your order #1042 is out for delivery with Bluedart. Expected by 4:00 PM.
				</div>
			</div>

			<!-- Call Controls -->
			<div class="flex items-center justify-between pt-2 border-t border-surface-800">
				<button 
					type="button" 
					class="p-2.5 rounded-full bg-surface-800 hover:bg-surface-700 text-slate-300 transition-colors"
					onclick={() => isMuted = !isMuted}
					title={isMuted ? "Unmute" : "Mute"}
				>
					{#if isMuted}
						<MicOff class="w-4 h-4 text-rose-400" />
					{:else}
						<Mic class="w-4 h-4" />
					{/if}
				</button>

				<button 
					type="button" 
					class="px-3 py-1.5 rounded-lg bg-surface-800 hover:bg-surface-700 text-[11px] font-medium text-slate-300 flex items-center gap-1.5 border border-surface-700"
					onclick={transferToHuman}
				>
					<UserCheck class="w-3.5 h-3.5 text-amber-400" />
					<span>Warm Transfer</span>
				</button>

				<button 
					type="button" 
					class="p-2.5 rounded-full bg-rose-600 hover:bg-rose-500 text-white transition-colors shadow-lg shadow-rose-600/30"
					onclick={endCall}
					title="Hang Up"
				>
					<PhoneOff class="w-4 h-4" />
				</button>
			</div>
		</div>
	{/if}
</div>
