/**
 * Panel-side query — given a row's identity (paneId and/or sessionUuid),
 * resolves to the pane breadcrumb, claude session, conversation, and
 * worktree (all daemon-joined).
 *
 * The Houdini operation lives at `lenses/houdini/OpenPanel.gql`. Tabs
 * hold `{ paneId, sessionUuid }`; this file resolves whichever is
 * supplied to a single `PanelData` shape.
 *
 * Worktree resolution: trust `session.worktree` (daemon-joined). The
 * conversation top-level is still queried because the panel surfaces
 * messageCount / open / recap / firstSeenAt + jsonlPath that the
 * SessionCard fragment doesn't carry.
 */
import { OpenPanelStore, type OpenPanel$result } from "$houdini";
import { parseTime } from "./client";
import type { PaneCardT, SessionCardT, WorktreeEnrichment } from "./fragments";

export interface PanelData {
	pane: PaneCardT | null;
	session: SessionCardT | null;
	conversation: {
		sessionUuid: string;
		lastSeenAt: number;
		firstSeenAt: number;
		messageCount: number;
		open: boolean;
		recap: string | null;
		recapSource: string | null;
		cwd: string | null;
		jsonlPath: string | null;
		agentName: string | null;
		customTitle: string | null;
	} | null;
	worktree: WorktreeEnrichment | null;
}

/** Spawn a fresh OpenPanel store. Each open panel gets its own. */
export function createPanelStore(): OpenPanelStore {
	return new OpenPanelStore();
}

type Data = NonNullable<OpenPanel$result>;

/**
 * Project the Houdini result into a single `PanelData` for the row
 * identity supplied. Pure — call inside `$derived` against
 * `$openPanelStore.data`.
 */
export function buildPanelData(
	data: Data | null | undefined,
	args: { paneId?: string | null; sessionUuid?: string | null },
): PanelData | null {
	if (!data) return null;
	const { paneId, sessionUuid } = args;
	if (!paneId && !sessionUuid) return null;

	const panes = data.tmuxPanes as unknown as PaneCardT[];
	const sessions = data.claudeInstances as unknown as SessionCardT[];

	// Resolve session first when we have a sessionUuid — it tells us which
	// pane to look for, so we don't have to guess `panes[0]`.
	let session: SessionCardT | null = null;
	if (sessionUuid) {
		session = sessions.find((s) => s.sessionUuid === sessionUuid) || null;
	}

	// Pick the pane:
	//   1. By explicit paneId from the row identity, if given.
	//   2. By the resolved session's paneId (daemon-joined ClaudeInstance.pane).
	//   3. As a last resort, the only pane the daemon returned (cwd+command
	//      filter narrowed it to one).
	let pane: PaneCardT | null = null;
	if (paneId) {
		pane = panes.find((p) => p.paneId === paneId) || null;
	}
	if (!pane && session?.pane?.paneId) {
		pane = panes.find((p) => p.paneId === session!.pane!.paneId) || null;
	}
	if (!pane && panes.length === 1) {
		pane = panes[0];
	}

	// Backfill session from pane when we didn't have a uuid up front.
	if (!session && pane?.claudeInstance) {
		session = sessions.find((s) => s.id === pane!.claudeInstance!.id) || null;
	}
	if (!session && paneId) {
		session = sessions.find((s) => s.pane?.paneId === paneId) || null;
	}

	const targetUuid = session?.sessionUuid || sessionUuid || null;
	const convRaw = targetUuid
		? data.conversations.find((c) => c.sessionUuid === targetUuid)
		: null;
	const conversation = convRaw
		? {
				sessionUuid: convRaw.sessionUuid,
				lastSeenAt: parseTime(convRaw.lastSeenAt),
				firstSeenAt: parseTime(convRaw.firstSeenAt),
				messageCount: convRaw.messageCount,
				open: convRaw.open,
				recap: convRaw.recap,
				recapSource: convRaw.recapSource ?? null,
				cwd: convRaw.cwd,
				jsonlPath: convRaw.jsonlPath,
				agentName: convRaw.agentName ?? null,
				customTitle: convRaw.customTitle ?? null,
			}
		: null;

	// Worktree from the daemon's join — no client-side cwd matching.
	const worktree =
		(session?.worktree as WorktreeEnrichment | undefined | null) ??
		(pane?.process?.worktree as WorktreeEnrichment | undefined | null) ??
		null;

	return { pane, session, conversation, worktree };
}
