/**
 * UI-side type aliases. Anything that comes from the daemon goes through
 * Houdini's generated `$houdini` types — this file is only for shapes the
 * UI itself owns (theme, surface, view enums, palette/chat/fork models).
 *
 * Earlier iterations of this file synthesized `Item` / `WorktreeItem` /
 * `ChannelItem` / `Agent` / `TerminalLine` / `PrInfo` etc. with hardcoded
 * defaults (sparkline=[], unread=0, ci="pending", load={cpu:0,mem:0,disk:0}).
 * Those types have been deleted; the real shape is the GraphQL schema, and
 * components that need daemon data import directly from `$houdini`.
 */
export type ConvView = "chat" | "terminal";

/**
 * The five lenses. Each is a separate Houdini query against its own anchor.
 *
 *   attention — claudeInstances + worktree enrichment, blocked/waiting/active
 *   recent    — claudeInstances sorted by lastActivityAt desc
 *   tmux      — tmuxServer.sessions[].windows[].panes[] tree
 *   issue     — worktree.issue rows where worktree.pr is OPEN/DRAFT
 *   worktree  — workView.repos[].worktrees[] grouped per repo
 */
export type Lens = "attention" | "recent" | "tmux" | "issue" | "worktree";
export type Theme = "dark" | "light";
export type Surface = "desktop" | "mobile";
export type SendStatus = "pending" | "sent" | "delivered" | "read";

/**
 * Chat-only message shape — these come out of the chat-core JSONL (orchard-chat),
 * not the daemon's GraphQL surface. Channel rooms render this; worktree session
 * panels render the Claude transcript via `TranscriptView` instead.
 */
export interface Message {
	id: string;
	role: "user" | "agent";
	agentId?: string;
	status: SendStatus;
	ts: number;
	text: string;
	tools?: string[];
	diff?: { plus: number; minus: number; files: number };
	isQuestion?: boolean;
	isPaused?: boolean;
}

export interface Conversation {
	itemId: string;
	recap: string;
	isChannel?: boolean;
	messages: Message[];
}

export interface ForkPreview {
	fromIdx: number;
	msg: Message;
}

/**
 * Palette entries. The palette is a flat list of jump targets; entries
 * pointing at daemon-backed nodes carry the row identity (paneId,
 * sessionUuid, roomId) the panel needs to open them.
 */
export type PaletteKind =
	| "session"
	| "channel"
	| "host"
	| "action"
	| "lens";

export interface PaletteEntry {
	kind: PaletteKind;
	label: string;
	sub?: string;
	host?: string;
	/** session targets — one or both populated. */
	paneId?: string;
	sessionUuid?: string;
	/** channel target — chat room id. */
	roomId?: string;
	group: string;
	keywords: string;
	shortcut?: string[];
	action?: string;
}
