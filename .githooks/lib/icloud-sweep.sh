#!/usr/bin/env bash
# iCloud conflict-copy sweep — the narrow version.
#
# The original swept the whole working tree for anything whose basename ended
# in " 2", " 3" (plus extension) and `rm -rf`'d it, on every commit, printing
# only a count. That destroys data by default: `* 2`/`* 3` is also how takes,
# versions, sections and dated exports are named. It deleted a tracked,
# LFS-stored Camtasia recording in peregrine-group-marketing —
# `Rec 4-26-2026 3.trec` — and staged the deletion (global-claude#324).
#
# Four rules, each removing one way it destroyed something:
#
#   1. NEVER a tracked file. git knows about it, so it is content, not a
#      sync artifact. This alone would have saved the .trec.
#   2. NEVER a directory. `rm -rf` on `Phase 2/` takes everything beneath it,
#      and the glob could not tell a directory from a file.
#   3. ONLY when the original exists beside it. A real iCloud conflict copy is
#      `X 2.ext` sitting next to `X.ext`; that sibling is the whole signature.
#      `Rec 4-26-2026 3.trec` has no `Rec 4-26-2026.trec` — it is a third take.
#   4. NAME what is removed. A count is unactionable by construction: you
#      cannot tell whether "removed 1 duplicate(s)" took a sync artifact or
#      your afternoon's work.
#
# Anything matching the pattern but failing 1–3 is REPORTED and left alone.
# Reporting a suspicious file costs a line; deleting a real one is unrecoverable
# when it is untracked.

icloud_sweep() {
    local removed=0 kept=0 path base dir stem ext original
    while IFS= read -r -d '' path; do
        # Rule 2 — directories are never swept.
        if [ -d "$path" ]; then
            printf '  kept (directory): %s\n' "$path"
            kept=$((kept + 1))
            continue
        fi
        # Rule 1 — tracked files are content.
        if git ls-files --error-unmatch "$path" >/dev/null 2>&1; then
            printf '  kept (tracked): %s\n' "$path"
            kept=$((kept + 1))
            continue
        fi
        # Rule 3 — require the original beside it.
        base=$(basename "$path")
        dir=$(dirname "$path")
        stem=${base% [23]}          # "notes 2"      -> "notes"
        ext=""
        if [ "$stem" = "$base" ]; then
            stem=${base%% [23].*}   # "notes 2.md"   -> "notes"
            ext=".${base##*.}"
        fi
        original="$dir/$stem$ext"
        if [ ! -e "$original" ]; then
            printf '  kept (no original at %s): %s\n' "$original" "$path"
            kept=$((kept + 1))
            continue
        fi
        rm -f "$path"
        printf '  removed iCloud conflict copy: %s\n' "$path"
        removed=$((removed + 1))
    done < <(find . -not -path './.git/*' \
        \( -name "* 2" -o -name "* 2.*" -o -name "* 3" -o -name "* 3.*" \) \
        -print0 2>/dev/null)
    [ "$removed" -eq 0 ] && [ "$kept" -eq 0 ] && return 0
    printf 'iCloud sweep: %d removed, %d kept\n' "$removed" "$kept"
    return 0
}
