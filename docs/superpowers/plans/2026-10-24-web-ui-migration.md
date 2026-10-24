# Web UI Migration Implementation Plan

> Execute inline using the executing-plans skill. User approved the web-only migration; no additional product decisions are required.

**Goal:** Continue development from artex-global with its existing Codex bridge and the enhanced web UI, without importing the macOS wrapper.

**Architecture:** Base the new feature/cryptolab-web-ui branch on 02dec1d. Import only the committed web/ delta from desktop revision 134bb12. Leave backend, database, bridge, dependencies and Docker deployment unchanged.

**Tech Stack:** Existing Go backend, Next.js/React frontend and Vitest tests.

**Spec:** The user-approved scope in this conversation: retain live panes, docking/resizing/focus, graph, themes and CryptoLab web branding; preserve existing uncommitted work and keep desktop implementation as an archive.

## Global Constraints
- No desktop/macos tree, native bundle or runtime update helper is imported.
- Preserve source checkout modifications and untracked experiment documents.
- Keep uncommitted Korean prompt changes separate.
- Target checkout writes require host filesystem permission; prepare and verify locally before transfer.

## Review Focus
- Same backend API/bridge source and package lock after import: compare tracked tree hashes.
- Browser operation without a native message handler: inspect all changed UI and run live-pane regression tests.
- Current docking, chat drafts, graph and auto-rotation behavior: existing web regression suite.
- Theme preview no longer writes into a nonexistent desktop tree: generate standalone HTML in web/out.
- Transfer preserves target dirty/untracked files and refuses conflicting changes: test installer against a disposable checkout.

## Tasks
- [x] Verify baseline web tests with existing dependencies.
- [x] Import the 50-file web patch; change only theme-lab default output to web/out/theme-lab.html.
- [x] Run web tests, TypeScript, static build and theme preview generation.
- [ ] Verify backend/dependency identity and no desktop additions; commit migration.
- [ ] Package a Git bundle and a guarded application command; test preservation/conflict behavior.
- [ ] Apply to original artex-global when filesystem permission is available; otherwise report this remaining step explicitly.

## Execution evidence

- Baseline: 7 Vitest files, 18 tests passed.
- Imported UI: 14 Vitest files, 80 tests passed; TypeScript passed.
- Production static export and standalone theme preview passed.
- Native theme messages are injected by Swift, not required by the browser frontend.
- Two web-only portability edits: theme preview output path and brand asset documentation.
- Package manifests and lockfiles, server, database and Codex bridge are unchanged.
- Korean prompt changes remain separately preserved at migration/korean-prompt-separate.patch outside this branch.
- Original artex-global checkout is unchanged: sandbox denies writes and elevated-command requests.
- Existing target dirty/untracked files must survive the eventual switch to this branch.
