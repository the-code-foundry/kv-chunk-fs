# Snapshot metadata, events, trees

## Metadata

Metadata is stored as extended attributes on the stub, readable with
`getfattr -d -m user.kvchunkfs <mount>/<model>/<file>`: `uses`, `last_use`, `created`
(Unix seconds), `label.<name>`, and `size`. It survives a rewrite and a rename over the
file (a proxy's `.tmp` → final), so a conversation keeps its history across turns.

A **use** is a read of at least 1 MiB (or half the file), counted at most once per
`USE_WINDOW` (30 s): llama-server restores a snapshot in two or three opens within a
second (a ~20 KB header check, the state, for some models the appended context
checkpoints — each open also reading the end of the file), which is one restore; the
header check alone is not a use.

## Events

Events go to the journal (`event=write file=… mode=stream bytes=… new_bytes=…
seconds=…`) and the log buffer (`GET /logs`): `write` (one per closed write; a model
with context checkpoints saves in two: state, then the appended checkpoints), `read`,
`delete` (with size, uses, age, idle), `gc`, `broken`, `error`.

## Token-prefix trees

Trees come from each snapshot's header: llama.cpp state format 4 and llama-server's
token envelope (`-1, 1, count, tokens…, media`). `GET /tree` groups snapshots by shared
token prefixes per model ("N snapshots share their first T tokens"). A file that does
not parse is listed as `unparsed: <reason>` (`not a llama.cpp slot file`, `format
version 5, expected 4`, `truncated header`, …); nothing fails because of it.
