<script lang="ts">
	// Inputs for a pipeline's custom fields, named f_<key> so fieldsFromForm can read them back.
	import { inputType, type FieldDef } from '$lib/crm';

	let { defs, values = {} }: { defs: FieldDef[]; values?: Record<string, string> } = $props();
	const cls = 'mt-1 w-full rounded-lg border border-line bg-surface px-3 py-2 text-sm';
</script>

{#each defs as f (f.key)}
	<label class="block text-sm">
		<span class="text-ink-2">{f.label}{#if f.required}<span class="text-bad" aria-hidden="true"> *</span>{/if}</span>
		{#if f.type === 'BOOLEAN'}
			<select name="f_{f.key}" class={cls} required={f.required}>
				<option value="" selected={!values[f.key]}>{f.required ? 'Choose…' : '—'}</option>
				<option value="true" selected={values[f.key] === 'true'}>Yes</option>
				<option value="false" selected={values[f.key] === 'false'}>No</option>
			</select>
		{:else}
			<input name="f_{f.key}" type={inputType(f)} step={f.type === 'NUMBER' ? 'any' : undefined} value={values[f.key] ?? ''} required={f.required} class={cls} />
		{/if}
	</label>
{/each}
