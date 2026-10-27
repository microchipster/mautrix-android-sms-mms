# Merged conversations

Android can fracture one real conversation into several telephony threads: group
messaging toggled on/off, a participant writing from a second number, a phone
restore that changed the thread ID before `rebind-room` was used, or Fossify bug
#863. Each thread owns a Matrix room, so the conversation ends up split.

`!sms merge` combines those rooms into one target room:

- every merged-away ("source") portal stores `MergedInto = <target portal ID>`,
- the target stores the reverse list `MergedFrom`,
- incoming events for a merged-away thread are bridged into the target room,
- the merged-away room gets an `m.room.tombstone` pointing at the target plus a
  notice.

The registry lives entirely in `PortalMetadata` (`merged_into`, `merged_from`),
so it survives restarts and needs no schema migration.

## Commands

```
!sms merge <room> <room> [<room> ...] [--into <room>] [--yes]
!sms merges
!sms unmerge <room>
```

Rooms can be given as Matrix room IDs (`!abc:example.com`), room aliases
(`#alias:example.com`, resolved through the Matrix connector) or bridge portal
IDs (`thread-<scope>-<sub>-<thread>`). The first room is the target unless
`--into` names another one; `--into` may also name a room not listed before it.

Examples:

```
!sms merge !old:example.com !current:example.com
!sms merge !a:example.com !b:example.com --into !b:example.com --yes
!sms merges
!sms unmerge !old:example.com
```

## Validation

Hard errors (the merge is refused):

- fewer than two rooms; the same room twice,
- a room that is not a bridge portal,
- a portal without a Matrix room yet,
- a Matrix space, or the management room,
- a target that is itself merged into another room (use `--into` with that
  room instead),
- a target that is not linked to a phone thread yet (replies would have nowhere
  to go).

Confirmation (`--yes`) is required when:

- more than two rooms are merged,
- the participant sets differ (for example a DM and the group it belongs to),
- a source is already merged somewhere else: `--yes` re-points it to the new
  target and removes it from the old target's list,
- a source is itself a merge target; its merged rooms follow the chain,
- the selected rooms do not all share a participant ("disjoint"): without
  `--yes` this is refused outright, with `--yes` it is forced.

The command always prints a per-room participant summary and the union before
it acts; without `--yes` it only prints that summary and asks for confirmation.

## Routing

- Incoming messages whose thread resolves to a merged-away portal are re-routed
  to the target: the target's participant union is refreshed (participants only
  accumulate until `unmerge`), the sender ghost is unchanged.
- `thread.updated` events for merged threads reconcile the target's recipients
  and room state (name, topic, members) instead of touching the tombstoned
  room.
- Replies and reactions sent in a merged-away room are still delivered to the
  target's phone thread, so the tombstoned room never becomes a dead end.
- At merge time the target's name and topic are rewritten to record the
  participant union (`Merged phone conversation (N phone threads): …`), and its
  room membership becomes the union.
- Chains (`A -> B -> C`) resolve to the final target when events are routed; the
  `merges` listing shows each direct hop.
- Broken registry entries (missing target, self-merge, cycles, chains longer
  than 8 hops) never route: events stay in their own room instead of being
  dropped.

## Unmerge

`!sms unmerge <room>` clears the room's `MergedInto` and removes it from the
target's `MergedFrom`. New messages from that thread are bridged into its own
room again.

The tombstone stays (Matrix has no "un-tombstone"), the target keeps the
participant union it had at merge time, and history is not moved.

## Known limitations (deferred)

- History rows are neither moved nor copied: pre-merge messages stay in the
  room they were bridged into, and new backfill for a merged-away portal is
  skipped so tombstoned rooms do not keep growing.
- Recipients only accumulate on the target; participants that leave a merged
  thread are not removed from the union automatically (unmerge is the escape
  hatch).
- Per-thread recipient reconciliation happens on `thread.updated`; a complete
  rebuild of the union from all merged phone threads is not implemented.
- No app-side multi-select UI yet: merging is a management-room command.
