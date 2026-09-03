package main

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Selection and viewport: which row is selected, and which part of the list
// is on screen. The two are deliberately separate — selection attaches a tmux
// session, scrolling does not — and both have to survive a list that re-sorts
// itself under the user every couple of seconds.

// selectRow moves the cursor to i and attaches that session. Selection IS the
// switch — there is no separate "jump" step and no cursor glyph, because the
// selected card is always the session you are looking at. handBack is
// switchClient's handBack, threaded straight through: true for a click/Enter
// (the user is done navigating, give the keyboard to the shell), false for
// j/k (still browsing, don't steal focus off the sidebar mid-move).
func (m *model) selectRow(i int, handBack bool) {
	r, ok := m.rowAt(i)
	if !ok {
		return
	}
	m.cursor = i
	m.cursorSess = r.session
	// the viewport follows a selection it cannot see, and only that: see
	// render's snap rule
	m.snapSel = true
	// Any client read already in flight predates this switch and would bounce
	// the bar back for a tick (the flicker); bumping the generation kills it.
	m.clientGen++
	// A switch is precisely when the lane must be fast again, however long it
	// had been idle — this covers both the keys and the click (#727).
	m.clientTick.reset()
	if r.fake {
		// no such tmux session: attaching would fail loudly (stderr into an
		// alt-screen pane) for a row that exists only to be scrolled past
		return
	}
	if handBack {
		// A deliberate choice (click/Enter) attaches this session, which makes
		// it the most-recently-attached one — so it belongs at the top now.
		// Promote it here rather than waiting for the sessions refresh to read
		// tmux's own last_attached back (~2s): the reorder lands in the same
		// frame as the snap, so the card moves up and the viewport follows it
		// there in one motion. j/k (handBack=false) does NOT promote — browsing
		// must not reshuffle the list under the person walking it.
		m.promote(r.session)
	}
	switchClient(r.session, handBack)
}

// promote marks a session as attached-now and re-sorts so it rises to the top
// immediately, keeping the cursor on it. The sessMeta bump is reconciled by
// the next sessions refresh, which reads tmux's own last_attached (also ~now
// after the switch-client) — so the optimistic value and the authoritative one
// agree and the card stays put rather than snapping back.
func (m *model) promote(session string) {
	if m.sessMeta == nil {
		m.sessMeta = map[string]sessMeta{}
	}
	meta := m.sessMeta[session]
	meta.lastAttached = time.Now()
	m.sessMeta[session] = meta
	m.applyOrder()
	sortRows(m.rows)
	m.reanchorCursor()
}

// scrollBy moves the viewport without touching the cursor. Selection is an
// attach (selectRow switches the client), so a wheel roll must not select:
// scrolling past a card and landing on it are different gestures. render()
// clamps, since only it knows how many list lines there are.
func (m *model) scrollBy(d int) {
	// a deliberate move retires the refresh anchor: the anchor exists to hold
	// the viewport still while data churns underneath it, and the user has
	// just said where the viewport goes
	m.anchorSess, m.anchorDelta = "", 0
	m.scroll += d
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// reanchorCursor keeps the cursor on the same session after a re-sort (rows
// reorder whenever a session changes state), falling back to the most recently
// active attached session before any user input has happened.
func (m *model) reanchorCursor() {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}
	// Which session the bar sits on is owned by the local tmux lane
	// (clientSessMsg) — it is per-client and ~20ms fresh, where the daemon's
	// attached flag is per-session and a poll behind. So this function no
	// longer picks a row; it only re-finds the row cursorSess moved to after a
	// re-sort, and falls back to something sane on first paint.
	if m.cursorSess != "" {
		for i, r := range m.rows {
			if r.session == m.cursorSess {
				m.cursor = i
				return
			}
		}
		// the session is known but its row hasn't been served yet (brand-new
		// session): keep the bar parked rather than walking to a card the
		// user never chose — that would also clobber cursorSess.
		m.cursor = -1
		return
	}
	// first paint, before the local lane has answered: prefer any session the
	// daemon believes is attached, else the top row.
	best := 0
	for i, r := range m.rows {
		if r.attached {
			best = i
			break
		}
	}
	m.cursor = best
	m.cursorSess = m.rows[best].session
}

// anchoredOffset re-derives the scroll offset so the same card stays at the
// top of the viewport across a refresh, even though the refresh re-sorted the
// rows and renumbered every line. Falls back to the raw offset when the
// anchored card is gone (the caller clamps it).
func (m *model) anchoredOffset(list []viewLine) int {
	// The top of the list is a POSITION, not a card. Parked at line 0 the user
	// is looking at "the start of the list", so a row arriving above the
	// anchored card has to appear — anchoring to the card instead pushed the
	// viewport down by exactly the new card's height to keep the old one
	// pinned, which silently hid every newly-arrived Needs-attention session
	// (and its section header) from anyone sitting at the top, which is where
	// this sidebar is normally left.
	if m.scroll == 0 {
		return 0
	}
	if m.anchorSess == "" {
		return m.scroll
	}
	for i, l := range list {
		if r, ok := m.rowAt(l.row); !ok || r.session != m.anchorSess {
			continue
		}
		return i + m.anchorDelta // i is that card's first line
	}
	return m.scroll
}

// key routes one key event. It deliberately does NOT switch on msg.String():
// bubbletea coalesces a burst of runes arriving in a single read into ONE
// KeyRunes message, so holding j down (or any fast repeat) produces
// msg.String() == "jjj", which matches no case and moves nothing — the list
// looks stuck exactly when the user is pressing hardest. Runes are handled one
// at a time; named keys are matched by Type so that Up/Down are the same code
// path as k/j rather than a parallel set of string cases that can drift apart.
func (m *model) key(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyCtrlC {
		return tea.Quit // the one key an open menu must not be able to swallow
	}
	if m.menuOpen() {
		return m.menuKey(msg) // instead of, not before: see menuKey
	}
	if m.updateOpen {
		m.closeUpdateOverlay() // any key dismisses it — there is nothing to navigate
		return nil
	}
	if msg.Alt && msg.Type == tea.KeyRunes {
		// M-1..M-9 is the one alt chord outer.conf forwards INTO this pane
		// (jump.go); every other M-x is the wrapper's and stops here.
		for _, r := range msg.Runes {
			if n, ok := jumpDigit(r); ok {
				m.jumpTo(n)
			}
		}
		return nil
	}
	if m.filter.open {
		return m.filterKey(msg) // instead of, not before, like menuKey
	}
	if msg.Type == tea.KeyRunes {
		var cmd tea.Cmd
		for i, r := range msg.Runes {
			if m.filter.open {
				// "/" opened the field midway through this burst — a fast
				// typist, or a paste, arrives as ONE KeyRunes message — so the
				// rest of the runes are text. Handing them to keyRune instead
				// swallowed everything typed in the same breath as the slash.
				m.filterKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: msg.Runes[i:]})
				break
			}
			if c := m.keyRune(r); c != nil {
				cmd = c
			}
		}
		return cmd
	}
	switch msg.Type {
	case tea.KeyDown:
		m.moveSel(1)
	case tea.KeyUp:
		m.moveSel(-1)
	case tea.KeyEnter:
		m.selectRow(m.railIndex(m.visibleRows()), true)
	case tea.KeyEsc:
		// a filter left applied by Enter is still dismissed by Esc, from the
		// list: otherwise the only way out is to reopen the field first
		m.clearFilter()
	}
	return nil
}

// keyRune handles one printable key. Every rune here has a named-key twin in
// key(): j/Down, k/Up — same call, same arguments, so the two ways of saying
// "next session" cannot behave differently.
func (m *model) keyRune(r rune) tea.Cmd {
	switch r {
	case 'q':
		return tea.Quit
	case 'j':
		m.moveSel(1)
	case 'k':
		m.moveSel(-1)
	case '/':
		m.openFilter()
	case 'b':
		m.toggleBell()
	}
	return nil
}

// moveSel walks the selection d cards through the VISIBLE list. Through the
// visible one because j/k must not step onto — and so attach — a card the
// filter is hiding; from railIndex rather than m.cursor for the same reason,
// since with the selected card filtered out the rail is where the user sees
// the selection. Off either end is a no-op, as it has always been.
//
// Unfiltered this is exactly what it replaces: every row is visible, railIndex
// is m.cursor, and "the next visible card" is the next index.
func (m *model) moveSel(d int) {
	vis := m.visibleRows()
	cur := m.railIndex(vis)
	at := -1
	for n, i := range vis {
		if i == cur {
			at = n
			break
		}
	}
	next := at + d
	if next < 0 || next >= len(vis) {
		return
	}
	m.selectRow(vis[next], false)
}
