<!--
  Chat scrollback + recap header + composer + fork preview. Auto-scrolls to
  bottom on message change.
-->
<script lang="ts">
	import { tick } from "svelte";
	import Icon from "$lib/icons/Icon.svelte";
	import ChatMessage from "./ChatMessage.svelte";
	import Composer from "./Composer.svelte";
	import type {
		Conversation,
		ForkPreview,
		Message,
	} from "$lib/data/types";

	type Props = {
		roomId: string;
		conversation: Conversation;
		surface: "desktop" | "mobile";
		now: number;
		statusVariant?: "ticks" | "dots" | "minimal" | "text";
		composeText: string;
		setComposeText: (s: string) => void;
		onSend: () => void;
		sending: { tempId: string; status: string } | null;
		forkPreview: ForkPreview | null;
		onStartFork: (idx: number, m: Message) => void;
		onCommitFork: () => void;
		onCancelFork: () => void;
	};
	let {
		roomId,
		conversation,
		surface,
		now,
		statusVariant = "ticks",
		composeText,
		setComposeText,
		onSend,
		sending,
		forkPreview,
		onStartFork,
		onCommitFork,
		onCancelFork,
	}: Props = $props();

	let recapOpen = $state(false);
	let scrollEl: HTMLDivElement | undefined = $state();

	$effect(() => {
		const _ = conversation.messages.length + (sending ? 1 : 0);
		void _;
		void roomId;
		tick().then(() => {
			if (scrollEl) scrollEl.scrollTop = scrollEl.scrollHeight;
		});
	});
</script>

<div class="chat">
	{#if conversation.recap}
		<div class="chat-recap" class:collapsed={!recapOpen} class:open={recapOpen}>
			<button
				class="chat-recap-toggle"
				onclick={() => (recapOpen = !recapOpen)}
				aria-expanded={recapOpen}
				title={recapOpen ? "Hide recap" : "Show recap"}
			>
				<Icon name="docs" size={11} />
				<span
					class="dimest mono"
					style="font-size: 10.5px; font-weight: 600; letter-spacing: 0.06em;"
				>
					RECAP
				</span>
				{#if !recapOpen}
					<span class="chat-recap-peek dimer">{conversation.recap}</span>
				{/if}
				<Icon name="chevron-down" size={11} />
			</button>
			{#if recapOpen}
				<p>{conversation.recap}</p>
			{/if}
		</div>
	{/if}

	<div class="chat-scroll" bind:this={scrollEl}>
		{#if conversation.messages.length === 0 && !sending}
			<div class="chat-empty dimer" style="text-align: center; padding: 32px 16px; font-size: 13px;">
				No messages in this room yet.
			</div>
		{/if}
		{#each conversation.messages as msg, i (msg.id)}
			{@const prev = conversation.messages[i - 1]}
			{@const grouped = !!(prev && prev.role === msg.role && msg.ts - prev.ts < 60_000 * 5)}
			<ChatMessage
				{msg}
				{grouped}
				isChannel={true}
				idx={i}
				{statusVariant}
				onForkFrom={(_i, m) => onStartFork(_i, m)}
				onReset={() => {}}
			/>
		{/each}

		{#if sending}
			<ChatMessage
				msg={{ id: "sending-typing", role: "agent", text: "", status: "pending", ts: now, typing: true } as Message & { typing: boolean }}
				grouped={false}
				isChannel={false}
				idx={conversation.messages.length}
				{statusVariant}
				onForkFrom={() => {}}
				onReset={() => {}}
			/>
		{/if}

		{#if forkPreview}
			<div class="chat-fork-preview fadeIn">
				<div class="chat-fork-header">
					<Icon name="git-fork" size={13} />
					<b>Forking from message #{forkPreview.fromIdx + 1}</b>
					<span class="dimer" style="font-size: 11px;">creates new session anchor</span>
				</div>
				<div class="chat-fork-actions">
					<button class="btn-ghost" onclick={onCancelFork}>Cancel</button>
					<button class="btn-primary" onclick={onCommitFork}>Fork conversation</button>
				</div>
			</div>
		{/if}
	</div>

	<Composer value={composeText} onChange={setComposeText} {onSend} {sending} {surface} {roomId} />
</div>
