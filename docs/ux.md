# NgiTool terminal UX rules

`internal/ui` implements these rules. Every command follows them. When a
command needs something the kit does not have, add it to the kit rather than
styling text by hand in the command.

## 1. One theme, semantic roles

Colours exist only in `internal/ui/theme.go`, as seven roles with adaptive
light/dark values:

| Role | Used for |
|---|---|
| `Accent` | the tool name, the focused option, answers, running spinners, info |
| `OK` | ✔ marks, success lines, "N selected" |
| `Warn` | ! marks, badges, warnings, the Explain panel |
| `Err` | ✖ marks and error messages |
| `Muted` | hints, notes, keys in plan blocks, help descriptions |
| `Key` | flags and commands the user should type |
| `Danger` | irreversible actions (bold) |

Commands call `ui.Accent(s)`, `ui.Muted(s)` and so on. A raw escape code or
lipgloss colour outside `theme.go` is a bug. Cursor control (hiding the
cursor, redrawing a spinner line) also lives only in `internal/ui`.

## 2. Plain output is the same output

Styling is off with `NO_COLOR` (any value), `TERM=dumb`, `--no-color`, or when
stdout is not a terminal. The text stays the same: same symbols (✔ ! ✖ ● ○ │
→), same layout, same words. Spinners and redraws are terminal-only: off a
terminal a task prints only its final ✔/✖ line and a step list prints one
line per finished step.

## 3. Width

- Widths are measured on visible cells (escape codes ignored, wide runes count
  two).
- Tables align every column and truncate only the last one to the terminal
  width, ending in `…`. A cut cell loses its styling rather than leave an
  escape sequence open.
- Under 60 columns (`ui.Narrow()`): option hints and tree hints drop first,
  labels never do; help puts each note on its own line under its command;
  the banner drops its tagline.

## 4. Components

| Component | Function | Shape |
|---|---|---|
| Banner | `Banner(version, status)` | `NgiTool  v0.1.0  <status line>` |
| Heading | `Heading(title, note)` | blank line, bold title, muted note |
| Table | `Table(rows, opts)` / `PrintTable` | aligned columns, last one truncated |
| Plan | `Plan(title, pairs)` | "here is what will happen": muted keys, values |
| Task | `Task(label, fn)` | spinner, elapsed seconds after 3 s, collapses to ✔/✖ + label |
| Steps | `Steps(title, steps)` | ✔ done · spinner running · ○ pending · – skipped after a failure |
| Callout | `Callout(kind, title, body)` | info ℹ, warn !, danger ✖, tip ★ with a coloured bar |
| Explain | `Explain(what, affects, undo)` | three rows: What it does / What it affects / How to undo |
| Diff | `UnifiedDiff(a, b, …)` + `Diff(text)` | unified diff, + green, − red, @@ accent |
| Tree | `Tree(node)` | server → location → upstream with ├─ └─ │ |
| Check line | `CheckLine(status, label, detail, fix)` | ✔/!/✖ label detail, `→ fix` below when not ok |

Log lines: `Step` (•), `Info` (ℹ), `Done` (✔), `Warning` (!), `Fail` (✖, to
stderr), `Hint` (muted, indented).

## 5. Prompts

All prompts are huh fields wrapped in one bubbletea model (`runField`) that
gives them the same behaviour:

- **Collapse.** A finished prompt leaves exactly one line:
  `✔ question › answer`.
- **Esc goes back** one level (`ui.ErrBack`). The root menu quits on Esc.
  While a filter box is open, Esc closes the filter first.
- **Ctrl-C** restores the terminal and exits 130 (`ui.ErrInterrupted`;
  outside prompts a SIGINT handler does the same).
- **Footer** of muted key hints under every prompt.

| Prompt | Function | Notes |
|---|---|---|
| Select | `Select(SelectOpts)` | label / badge / hint columns; `Disabled: "reason"` rows show the reason and cannot be picked; `Sep("Group")` headings |
| Filterable select | `SelectOpts{Filter: true}` | opens with the type-to-filter box; `/` opens it in any select |
| Manual entry | `SelectOpts{Manual: &Manual{…}}` | appends `✎ Enter it manually…`, which opens a validated input; Esc from it returns to the list |
| Multiselect | `MultiSelect(MultiOpts)` | space toggles, `a` all, `n` none, live "N selected"; a heading toggles its whole group; disabled rows never tick |
| Input | `Input(InputOpts)` | validation runs as you type; `Default` fills an empty answer; `Secret` masks |
| Confirm | `Confirm(question, note, default)` | yes/no |
| Danger confirm | `ConfirmTyped(question, name)` | the user types the resource's name |

**Typing is the last resort.** Every list NgiTool builds from what it found
(instances, containers, compose services, hosts) ends with the manual-entry
row. A bare text input as the first step of a flow is a design bug.

## 6. Non-interactive rule

Every prompt has a flag. Commands ask through three helpers in `ui/ask.go`:

- `Need(what, flag)` before asking for a value: off a terminal it returns a
  `MissingError` ("missing shell: pass bash|zsh|fish …"), which the CLI prints
  with the help command and exit code 2.
- `Sure(yes, …)` for confirmations: `--yes` skips it; off a terminal without
  `--yes` it fails naming `--yes` (it never assumes yes).
- `SureDanger(yes, force, …)` for irreversible actions: only `--yes --force`
  together skip the typed confirm.

`--json` on every list or inspect command prints only JSON (no styling, no
spinner, no update notice) on stdout.

## 7. Errors and exit codes

| Code | Meaning |
|---|---|
| 0 | done |
| 1 | failed, or cancelled (Esc / "No") — "cancelled — nothing changed" |
| 2 | usage: unknown command or flag, missing value off a terminal |
| 10 | `update --check`: a newer release exists |
| 130 | interrupted with Ctrl-C |

Errors print as `✖ first line`, then the remaining lines muted and indented
(for example the last 12 lines of a failed command's output). An error says
what to do next whenever there is a next step: the flag to pass, the command
to run, the file to fix.

## 8. Before changing anything

A mutating flow shows a Plan block, an Explain panel when the action is
destructive or easy to misunderstand (restart, recreate, rebuild, down,
downgrade, purge), then asks. It holds the state lock while it writes.

## 9. Instances

`scan` shows one card per nginx; `instances` is the compact table; `inspect`
is a Plan block plus a Tree.

- **Kind icon**: ◆ edge · ⌂ host · ◫ compose · ▣ container.
- **State dot**: `OK` ● running · `Warn` ● stopped · `Muted` ○ defined.
- **Badges**: `[front door]` in `OK`; another manager in `Warn` (`[swag]`).
- **Capability chips**: ✔ read ✔ test ✔ reload ✔ write in `OK`, or ○ in
  `Muted` with `name: reason` on its own line below. A true capability that
  could be misread (a `:ro` mount, a single-file mount) gets a muted note.
- **Findings** are grouped Errors, Warnings, Notes; each is a check line
  whose fix hint follows `→`, with the edge-case ID muted at the end.
- **Tree targets** are coloured by reachability: `OK` reachable, `Err`
  missing or no shared network, `Warn` nothing listening; unknown stays plain.
  Every node's hint ends with its file:line.

## 10. Routes, pools and changes

- **Every change** shows a Plan block (what will exist), then the diff
  grouped by file with one summary line ("2 files changed, 1 added, 0
  removed"), then asks. Removals show the cascade first and an Explain panel.
- **Edge-case IDs** end every refusal and warning, muted: `… (LB-02)`; the
  fix follows on its own line after `→`.
- **Health**: `OK` ● reached its upstream, `Err` ✖ answered 502/503/504 or
  did not answer, `Muted` ○ not probed yet. The symbols differ so plain
  output keeps the meaning.
- **`route ls`** is a Tree per instance: host (TLS and port-80 mode as the
  hint) → path → target or pool summary, with the last probe's status.
- **The wizard** types only the hostname; everything else is a select or
  the target checklist, whose rows are cut to one line (never wrapped) and
  list targets on a shared network first.
- **nginx -t failures** print nginx's lines muted, then the named file:line
  with two lines around it and the offending line marked `❯` in `Err`.
