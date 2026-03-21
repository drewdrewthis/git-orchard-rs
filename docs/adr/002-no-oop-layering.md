# ADR-002: No OOP Service Layers or Dependency Injection

## Status
Accepted

## Date
2026-03-21

## Context

Orchard is a ~3k-line Rust TUI that shells out to git, tmux, and the gh CLI. The codebase uses plain structs for data, free functions for behavior, and flat modules for grouping (`git.rs`, `tmux.rs`, `github.rs`).

A reasonable question arises: should we introduce trait-based dependency injection, service layers, or repository patterns to "start correctly" before the codebase grows?

## Decision

No. Keep the current architecture: data structs, free functions, and modules.

### Rationale

1. **Free functions are the easiest thing to refactor.** Moving `git::get_worktrees()` behind a `trait GitProvider` is a small change when actually needed (e.g., for test mocks or swapping to libgit2). Doing it preemptively means every caller pays the abstraction tax for zero benefit.

2. **Rust modules already provide the separation that service classes provide in OOP languages.** `git.rs` gives the same boundary as `GitService.java` — without the ceremony. Adding a `services/` directory would add a folder, not clarity.

3. **DI has real costs in Rust.** Trait objects add vtable dispatch and lifetime complexity. Generics add monomorphization bloat and viral type parameters. Neither is free, and neither is justified unless you're swapping implementations at runtime or in tests.

4. **The "what if it grows?" instinct is the YAGNI trap.** You refactor when you feel friction, not in anticipation of it. Pre-building layers that serve no current need is cargo-culting — replicating the form of enterprise Java patterns without the underlying reasons that justify them (500k+ line codebases with runtime-swappable implementations).

5. **The current architecture already follows good separation.** I/O lives at the edges (`git.rs`, `tmux.rs`), composition lives in the pipeline (`collector/`), and presentation lives in `tui/`. This is the idiomatic Rust approach.

### When to revisit

Introduce trait abstractions when one of these becomes true:

- **Test mocks are needed.** If testing a function requires faking git/tmux output and subprocess stubbing is too painful, extract a trait for that boundary.
- **Multiple backends exist.** If we support both subprocess-based and library-based git operations, a trait is the natural seam.
- **The module exceeds ~500 lines of related functions.** Split the module, but into smaller modules — not into trait + impl pairs.

## Consequences

- New contributors familiar with OOP may initially look for service classes. This ADR explains why they don't exist.
- Refactoring to traits later is straightforward because free functions already have clean signatures — the function signature *is* the trait method signature.
- The codebase stays small, direct, and easy to navigate.
