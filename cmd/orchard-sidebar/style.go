package main

import "github.com/charmbracelet/lipgloss"

// Every colour and style the pane draws with, in one place — the palette is a
// whole-UI decision (which hue means "your move", what the selected card lifts
// to) and reads as one only when it is written as one.

// neonAccent lights up the selected card — its name and the section title it
// sits under — so the eye finds the attached session in one jump. A truecolor
// hex rather than an ANSI index: the 16-color palette has no neon, and every
// one of its slots is already spoken for by a session state.
const neonAccent = "#00F0FF"

// neonPurple is the selected card's thick left border — the border is the
// focus signal, and only the attached session gets one.
const neonPurple = "#BF00FF"

// paleNeon is the selected card's body text — a pale tint of neonAccent, so
// the active card's details read clearly without competing with the name.
const paleNeon = "#A8F0FF"

var (
	stySelBar  = lipgloss.NewStyle().Foreground(lipgloss.Color(neonPurple)).Bold(true)
	stySelName = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	stySelHead = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	stySelAge  = lipgloss.NewStyle().Foreground(lipgloss.Color(neonAccent)).Bold(true)
	styErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	styDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styPrompt  = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("8"))
	// the selected card's body lifts out of the dim gray the rest of the list
	// sits in — a pale tint of the accent, so name/border still lead. The
	// prompt quote stays gray (just brighter than inactive): it's the one
	// body line that's prose, not chrome, and the tint read as over-styled
	stySelBody   = lipgloss.NewStyle().Foreground(lipgloss.Color(paleNeon))
	stySelPrompt = lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("7"))

	// bucket colours for the state dot. Adaptive so the same marker stays
	// legible on a light terminal, where a bright 256-colour value washes out:
	// the Light variants are the darker end of each hue.
	styAttn = lipgloss.NewStyle().Bold(true).Foreground(
		lipgloss.AdaptiveColor{Light: "130", Dark: "214"})
	styDone = lipgloss.NewStyle().Bold(true).Foreground(
		lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	styWork = lipgloss.NewStyle().Bold(true).Foreground(
		lipgloss.AdaptiveColor{Light: "26", Dark: "45"})
)

// marker returns the one-cell state dot and the style to draw it in. Colour
// carries the bucket (amber = your move, green = a result waiting, blue = still
// running, gray = nothing to say), which is why it comes back with the glyph
// rather than being picked at the call site. frame spins the working dot.
//
// Colours are 256-colour, chosen per background: nothing here assumes truecolor
// and nothing here is legible only on black.
func marker(r row, frame int) (string, lipgloss.Style) {
	switch rowBucket(r) {
	case bucketAttention:
		if r.state == "stalled" {
			return "✖", styAttn
		}
		return "●", styAttn
	case bucketDone:
		return "✓", styDone
	}
	switch r.state {
	case "working":
		return workFrames[frame%len(workFrames)], styWork
	case "shell":
		return "·", styDim
	}
	return "○", styDim
}
