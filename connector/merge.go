// SPDX-FileCopyrightText: 2026 Slavi Pantaleev
//
// SPDX-License-Identifier: AGPL-3.0-or-later

package connector

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	agentapi "mautrix-android-sms-mms/internal/agent"
)

// Merged conversations: Android can fracture one real conversation into several
// telephony threads (e.g. group messaging toggled on/off, a participant using a
// second number, a phone restore that changed the thread ID before rebind-room
// was used). Each thread owns a Matrix room, so the conversation ends up split
// across rooms. A merge combines them:
//
//   - every source portal stores MergedInto = target portal ID,
//   - the target stores the reverse list MergedFrom,
//   - incoming events for a merged-away thread are routed to the target room,
//   - the merged-away room gets an m.room.tombstone pointing at the target.
//
// The registry lives entirely in PortalMetadata, so it survives restarts and
// needs no schema migration.

const (
	// maxMergeChainLength bounds how many MergedInto links are followed before
	// the registry is treated as broken. It guards against cycles.
	maxMergeChainLength = 8

	mergeUsage   = "merge <room-id> <room-id> [<room-id> ...] [--into <room-id>] [--yes]"
	unmergeUsage = "unmerge <room-id>"
)

// portalMetadataOf returns a portal's metadata, creating it when missing.
func portalMetadataOf(portal *bridgev2.Portal) *PortalMetadata {
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil {
		meta = &PortalMetadata{}
		portal.Metadata = meta
	}
	return meta
}

// portalParticipantList returns the canonical participant list stored in a
// portal's metadata, falling back to the DM counterpart.
func portalParticipantList(meta *PortalMetadata) []string {
	if meta == nil {
		return nil
	}
	if len(meta.Recipients) > 0 {
		return CanonicalRecipients(meta.Recipients)
	}
	if meta.OtherUserID != "" {
		return CanonicalRecipients([]string{UnescapeAddress(string(meta.OtherUserID))})
	}
	return nil
}

// mergeSortedIDs returns a deduplicated, sorted copy of the given portal IDs.
func mergeSortedIDs(ids []networkid.PortalID) []networkid.PortalID {
	seen := make(map[networkid.PortalID]struct{}, len(ids))
	out := make([]networkid.PortalID, 0, len(ids))
	for _, portalID := range ids {
		if portalID == "" {
			continue
		}
		if _, ok := seen[portalID]; ok {
			continue
		}
		seen[portalID] = struct{}{}
		out = append(out, portalID)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mergeRemoveID returns the list without the given portal ID.
func mergeRemoveID(ids []networkid.PortalID, drop networkid.PortalID) []networkid.PortalID {
	out := make([]networkid.PortalID, 0, len(ids))
	for _, portalID := range ids {
		if portalID != drop {
			out = append(out, portalID)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveMergeTarget follows MergedInto links in the merge registry. lookup
// returns the metadata for a portal ID (nil when the portal is unknown). It
// returns the final target and true when the start portal is merged away.
//
// Broken registry entries (missing target, self-merge, cycles, chains longer
// than maxMergeChainLength) report false so events stay in the source portal
// instead of being routed nowhere.
func resolveMergeTarget(start networkid.PortalID, lookup func(networkid.PortalID) *PortalMetadata) (networkid.PortalID, bool) {
	if start == "" || lookup == nil {
		return "", false
	}
	visited := map[networkid.PortalID]struct{}{start: {}}
	current := start
	for hop := 0; hop < maxMergeChainLength; hop++ {
		meta := lookup(current)
		if meta == nil || meta.MergedInto == "" {
			return "", false
		}
		next := meta.MergedInto
		if next == current {
			return "", false
		}
		if _, seen := visited[next]; seen {
			return "", false
		}
		visited[next] = struct{}{}
		nextMeta := lookup(next)
		if nextMeta == nil {
			return "", false
		}
		if nextMeta.MergedInto == "" {
			return next, true
		}
		current = next
	}
	return "", false
}

// mergeRoom is the resolved state of one room selected in a merge command.
// The pure validation helpers only read this struct; Portal is used by the
// command layer to save the merged state.
type mergeRoom struct {
	// Input is what the user typed (room ID, alias or portal ID).
	Input string
	// RoomID is the Matrix room, empty when the portal has no room yet.
	RoomID id.RoomID
	// PortalID is the bridge portal ID.
	PortalID networkid.PortalID
	// Name is the room name, used in summaries.
	Name string
	// Recipients is the canonical participant list.
	Recipients []string
	// ThreadID is the phone thread the portal is linked to (0 when unknown).
	ThreadID int64
	// MergedInto is set when the portal is already merged away.
	MergedInto networkid.PortalID
	// MergedFromCount is how many portals have been merged into this one.
	MergedFromCount int
	// IsSpace marks Matrix spaces (they are not conversations).
	IsSpace bool
	// IsManagementRoom marks the bridge management room.
	IsManagementRoom bool

	// Portal is the loaded portal (nil in pure tests).
	Portal *bridgev2.Portal
}

// mergePlan is a validated merge of mergeRooms into a target.
type mergePlan struct {
	Rooms  []mergeRoom
	Target int
	// Sources are indexes into Rooms of the portals that will be merged away.
	Sources []int
	// AlreadyMerged lists sources that are already merged into the target.
	AlreadyMerged []int
	// Participants is the canonical union of every selected room.
	Participants []string
	// Disconnected is true when the selected rooms do not form one participant
	// graph (some room shares no participant with any other). It is refused
	// unless the caller forces the merge with --yes.
	Disconnected bool
	// ConfirmationReasons explains why --yes is required.
	ConfirmationReasons []string
}

// RequiresConfirmation reports whether the caller must pass --yes.
func (p *mergePlan) RequiresConfirmation() bool {
	return len(p.ConfirmationReasons) > 0
}

// planMerge validates a merge selection. Hard errors cover selections that can
// never work (too few rooms, non-portals, spaces, the management room, a target
// that is itself merged away); confirmation gates cover merges that are
// structurally surprising but valid (more than two rooms, different participant
// sets, re-pointing an already merged room, disjoint participant sets).
func planMerge(rooms []mergeRoom, target int) (*mergePlan, error) {
	if len(rooms) < 2 {
		return nil, fmt.Errorf("select at least two rooms to merge. Usage: %s", mergeUsage)
	}
	if target < 0 || target >= len(rooms) {
		return nil, fmt.Errorf("the merge target is not one of the selected rooms")
	}
	seen := make(map[networkid.PortalID]struct{}, len(rooms))
	for i, room := range rooms {
		label := mergeRoomLabel(room, i)
		if room.PortalID == "" {
			return nil, fmt.Errorf("%s is not a bridge portal", label)
		}
		if _, duplicate := seen[room.PortalID]; duplicate {
			return nil, fmt.Errorf("%s was selected twice", label)
		}
		seen[room.PortalID] = struct{}{}
		if room.IsManagementRoom {
			return nil, fmt.Errorf("%s is the management room", label)
		}
		if room.IsSpace {
			return nil, fmt.Errorf("%s is a space, not a conversation", label)
		}
		if room.RoomID == "" {
			return nil, fmt.Errorf("%s has no Matrix room yet", label)
		}
	}
	if rooms[target].MergedInto != "" {
		return nil, fmt.Errorf("%s is itself merged into %s; use --into with the target room instead",
			mergeRoomLabel(rooms[target], target), rooms[target].MergedInto)
	}
	if rooms[target].ThreadID == 0 {
		return nil, fmt.Errorf("%s is not linked to a phone thread yet", mergeRoomLabel(rooms[target], target))
	}

	plan := &mergePlan{Rooms: rooms, Target: target}
	for i, room := range rooms {
		if i == target {
			continue
		}
		label := mergeRoomLabel(room, i)
		if room.MergedInto == rooms[target].PortalID {
			plan.AlreadyMerged = append(plan.AlreadyMerged, i)
			continue
		}
		if room.MergedInto != "" {
			plan.ConfirmationReasons = append(plan.ConfirmationReasons,
				fmt.Sprintf("%s is already merged into %s; --yes re-points it", label, room.MergedInto))
		}
		if room.MergedFromCount > 0 {
			plan.ConfirmationReasons = append(plan.ConfirmationReasons,
				fmt.Sprintf("%s is itself a merge target; its merged rooms follow the chain", label))
		}
		plan.Sources = append(plan.Sources, i)
	}

	participantSets := make([][]string, 0, len(rooms))
	for _, room := range rooms {
		participantSets = append(participantSets, room.Recipients)
	}
	plan.Participants = mergeRecipientUnion(participantSets...)

	if len(rooms) > 2 {
		plan.ConfirmationReasons = append(plan.ConfirmationReasons,
			fmt.Sprintf("merging %d rooms", len(rooms)))
	}
	if !mergeParticipantSetsEqual(participantSets) {
		plan.ConfirmationReasons = append(plan.ConfirmationReasons,
			"the rooms have different participant sets")
	}
	if mergeComponentCount(participantSets) > 1 {
		plan.Disconnected = true
		plan.ConfirmationReasons = append(plan.ConfirmationReasons,
			"the selected rooms do not all share a participant")
	}
	return plan, nil
}

// mergeRecipientUnion returns the canonical union of the given participant
// lists.
func mergeRecipientUnion(sets ...[]string) []string {
	all := make([]string, 0)
	for _, set := range sets {
		all = append(all, set...)
	}
	if len(all) == 0 {
		return nil
	}
	return CanonicalRecipients(all)
}

// mergeParticipantSetsEqual reports whether every set holds the same
// participants (sets are canonical and sorted, so slices.Equal is exact).
func mergeParticipantSetsEqual(sets [][]string) bool {
	for i := 1; i < len(sets); i++ {
		if !slices.Equal(sets[0], sets[i]) {
			return false
		}
	}
	return true
}

// mergeComponentCount returns the number of connected components of the room
// graph, where two rooms are connected when they share at least one
// participant (a contact or an own number).
func mergeComponentCount(participantSets [][]string) int {
	n := len(participantSets)
	if n == 0 {
		return 0
	}
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b int) {
		rootA, rootB := find(a), find(b)
		if rootA != rootB {
			parent[rootB] = rootA
		}
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if mergeSetsOverlap(participantSets[i], participantSets[j]) {
				union(i, j)
			}
		}
	}
	components := make(map[int]struct{}, n)
	for i := range parent {
		components[find(i)] = struct{}{}
	}
	return len(components)
}

// mergeSetsOverlap reports whether two participant lists share an entry.
func mergeSetsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, participant := range a {
		set[participant] = struct{}{}
	}
	for _, participant := range b {
		if _, ok := set[participant]; ok {
			return true
		}
	}
	return false
}

// mergeRoomLabel renders one room for command output.
func mergeRoomLabel(room mergeRoom, index int) string {
	switch {
	case room.Name != "" && room.RoomID != "":
		return fmt.Sprintf("%q (%s)", room.Name, room.RoomID)
	case room.Name != "":
		return fmt.Sprintf("%q", room.Name)
	case room.RoomID != "":
		return string(room.RoomID)
	case room.Input != "":
		return fmt.Sprintf("%q", room.Input)
	default:
		return fmt.Sprintf("room #%d", index+1)
	}
}

// mergeEntryRoomLabel renders a room for the `merges` listing.
func mergeEntryRoomLabel(room mergeRoom) string {
	switch {
	case room.Name != "" && room.RoomID != "":
		return fmt.Sprintf("%s (%s)", room.Name, room.RoomID)
	case room.RoomID != "":
		return string(room.RoomID)
	case room.PortalID != "":
		return string(room.PortalID)
	default:
		return "(unknown)"
	}
}

// mergeParticipantSummary renders a participant list for command output.
func mergeParticipantSummary(participants []string) string {
	if len(participants) == 0 {
		return "(no participants known)"
	}
	return groupName(participants)
}

// formatMergePlanSummary renders the participant summary shown before a merge
// is confirmed.
func formatMergePlanSummary(plan *mergePlan) string {
	if plan == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rooms in this merge (%d):", len(plan.Rooms))
	for i, room := range plan.Rooms {
		role := "source"
		if i == plan.Target {
			role = "target"
		} else if slices.Contains(plan.AlreadyMerged, i) {
			role = "already merged"
		}
		fmt.Fprintf(&b, "\n- [%s] %s: %s", role, mergeRoomLabel(room, i), mergeParticipantSummary(room.Recipients))
	}
	fmt.Fprintf(&b, "\nUnion participants: %s", mergeParticipantSummary(plan.Participants))
	return b.String()
}

// mergeTargetName renders the room name for a merged target.
func mergeTargetName(participants []string, displayName func(address string) string) string {
	switch len(participants) {
	case 0:
		return "Unknown conversation"
	case 1:
		if displayName != nil {
			if name := strings.TrimSpace(displayName(participants[0])); name != "" {
				return name
			}
		}
		return participants[0]
	default:
		return groupName(participants)
	}
}

// mergeTargetTopic renders the topic recording the participant union at merge
// time.
func mergeTargetTopic(threadCount int, participants []string) string {
	if threadCount < 1 {
		threadCount = 1
	}
	if len(participants) == 0 {
		return fmt.Sprintf("Merged phone conversation (%d phone threads)", threadCount)
	}
	return fmt.Sprintf("Merged phone conversation (%d phone threads): %s",
		threadCount, groupName(participants))
}

// mergedChatInfo builds the full desired room state for a merged target.
func mergedChatInfo(ctx context.Context, br *bridgev2.Bridge, participants []string, topic string) *bridgev2.ChatInfo {
	roomType := database.RoomTypeDefault
	if len(participants) == 1 {
		roomType = database.RoomTypeDM
	}
	name := mergeTargetName(participants, func(address string) string {
		if br == nil {
			return CanonicalAddress(address)
		}
		ghost, err := br.GetExistingGhostByID(ctx, MakeUserID(address))
		if err != nil || ghost == nil {
			return CanonicalAddress(address)
		}
		meta, _ := ghost.Metadata.(*GhostMetadata)
		return GhostDisplayName(meta, address)
	})
	members := &bridgev2.ChatMemberList{
		IsFull: true,
		Members: []bridgev2.ChatMember{{
			EventSender: bridgev2.EventSender{IsFromMe: true},
			Membership:  event.MembershipJoin,
		}},
	}
	for _, participant := range participants {
		members.Members = append(members.Members, bridgev2.ChatMember{
			EventSender: bridgev2.EventSender{Sender: MakeUserID(participant)},
			Membership:  event.MembershipJoin,
		})
	}
	if len(participants) == 1 {
		members.OtherUserID = MakeUserID(participants[0])
	}
	return &bridgev2.ChatInfo{
		Name:        &name,
		Topic:       &topic,
		Type:        &roomType,
		Members:     members,
		CanBackfill: true,
	}
}

// mergeThreadIdentity rewrites a source thread's identity to the target's so
// routing never clobbers the target's phone thread mapping.
func mergeThreadIdentity(meta *PortalMetadata, thread agentapi.Thread) agentapi.Thread {
	if meta == nil {
		return thread
	}
	if meta.ThreadID != 0 {
		thread.ID = meta.ThreadID
	}
	if meta.SubID != 0 {
		thread.SubID = meta.SubID
	}
	return thread
}

// mergeAccumulateRecipients unions a thread's participants into the stored
// participant set of a merge target. Plain portals keep replace semantics.
func mergeAccumulateRecipients(meta *PortalMetadata, thread agentapi.Thread) agentapi.Thread {
	if meta == nil || len(meta.MergedFrom) == 0 {
		return thread
	}
	thread.Recipients = mergeRecipientUnion(meta.Recipients, thread.Recipients)
	return thread
}

// mergeRoutedThread rewrites a merged-away thread so its event lands in the
// target: target thread identity plus the participant union.
func mergeRoutedThread(meta *PortalMetadata, thread agentapi.Thread) agentapi.Thread {
	if meta == nil {
		return thread
	}
	thread = mergeThreadIdentity(meta, thread)
	thread.Recipients = mergeRecipientUnion(meta.Recipients, thread.Recipients)
	return thread
}

// mergeRoute maps an incoming event's portal to its merge target. It returns
// the key the event belongs to, the metadata of that portal (nil when the
// portal is unknown) and whether the event was routed across a merge.
func (nc *MyNetworkClient) mergeRoute(ctx context.Context, key networkid.PortalKey) (networkid.PortalKey, *PortalMetadata, bool) {
	portal, homeKey, merged := nc.resolveMergedPortal(ctx, key)
	if portal == nil {
		return key, nil, false
	}
	meta, _ := portal.Metadata.(*PortalMetadata)
	return homeKey, meta, merged
}

// resolveMergedPortal follows the merge registry from a portal key. It returns
// the portal events should be routed to (the target when the key was merged
// away), the key of that portal, and whether the key was merged away.
func (nc *MyNetworkClient) resolveMergedPortal(ctx context.Context, key networkid.PortalKey) (*bridgev2.Portal, networkid.PortalKey, bool) {
	if nc == nil || nc.bridge == nil {
		return nil, key, false
	}
	portal, err := nc.bridge.GetExistingPortalByKey(ctx, key)
	if err != nil || portal == nil {
		return nil, key, false
	}
	lookup := func(portalID networkid.PortalID) *PortalMetadata {
		target, err := nc.bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: portalID})
		if err != nil || target == nil {
			return nil
		}
		meta, _ := target.Metadata.(*PortalMetadata)
		return meta
	}
	targetID, merged := resolveMergeTarget(key.ID, lookup)
	if !merged {
		return portal, key, false
	}
	target, err := nc.bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: targetID})
	if err != nil || target == nil {
		return nil, key, false
	}
	return target, target.PortalKey, true
}

// mergedTargetForPortal resolves an already loaded portal to its merge target.
func (nc *MyNetworkClient) mergedTargetForPortal(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.Portal, *PortalMetadata, bool) {
	if nc == nil || portal == nil {
		return nil, nil, false
	}
	meta, _ := portal.Metadata.(*PortalMetadata)
	if meta == nil || meta.MergedInto == "" {
		return nil, nil, false
	}
	target, _, merged := nc.resolveMergedPortal(ctx, portal.PortalKey)
	if !merged || target == nil {
		return nil, nil, false
	}
	targetMeta, _ := target.Metadata.(*PortalMetadata)
	return target, targetMeta, true
}

// mergedPortalMetadata returns the metadata of the portal an input portal
// routes to when it is merged away, or nil. The Matrix -> phone direction uses
// it so replies in a merged-away room reach the target thread.
func (nc *MyNetworkClient) mergedPortalMetadata(ctx context.Context, portal *bridgev2.Portal) *PortalMetadata {
	_, targetMeta, merged := nc.mergedTargetForPortal(ctx, portal)
	if !merged || targetMeta == nil {
		return nil
	}
	return targetMeta
}

// mergeRequest is the parsed `merge` command line.
type mergeRequest struct {
	Rooms []string
	Into  string
	Yes   bool
}

// parseMergeRequest parses `merge <room> <room> [...] [--into <room>] [--yes]`.
func parseMergeRequest(args []string) (mergeRequest, error) {
	var req mergeRequest
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		switch {
		case arg == "":
			continue
		case arg == "--yes":
			req.Yes = true
		case arg == "--into":
			if i+1 >= len(args) {
				return req, fmt.Errorf("--into needs a room. Usage: %s", mergeUsage)
			}
			i++
			req.Into = strings.TrimSpace(args[i])
			if req.Into == "" {
				return req, fmt.Errorf("--into needs a room. Usage: %s", mergeUsage)
			}
		case strings.HasPrefix(arg, "--"):
			return req, fmt.Errorf("unknown option %q. Usage: %s", arg, mergeUsage)
		default:
			req.Rooms = append(req.Rooms, arg)
		}
	}
	if len(req.Rooms) == 0 {
		return req, fmt.Errorf("Usage: %s", mergeUsage)
	}
	if len(req.Rooms) == 1 && req.Into == "" {
		return req, fmt.Errorf("select at least two rooms to merge. Usage: %s", mergeUsage)
	}
	return req, nil
}

// mergeRoomMatchesRef reports whether a resolved room matches a reference the
// user typed (used to bind --into to a positional room).
func mergeRoomMatchesRef(room mergeRoom, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	return ref == string(room.RoomID) || ref == string(room.PortalID) || ref == strings.TrimSpace(room.Input)
}

// resolveMergeRoom turns a room ID, a room alias or a portal ID into a
// mergeRoom.
func resolveMergeRoom(ctx context.Context, br *bridgev2.Bridge, ref string) (mergeRoom, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return mergeRoom{}, fmt.Errorf("room reference cannot be empty")
	}
	if br == nil || br.DB == nil {
		return mergeRoom{}, fmt.Errorf("bridge is not available")
	}
	var portal *bridgev2.Portal
	var err error
	switch {
	case strings.HasPrefix(ref, "#"):
		mxid, aliasErr := resolveRoomAlias(ctx, br, id.RoomAlias(ref))
		if aliasErr != nil {
			return mergeRoom{}, aliasErr
		}
		portal, err = br.GetPortalByMXID(ctx, mxid)
	case strings.HasPrefix(ref, "!"):
		portal, err = br.GetPortalByMXID(ctx, id.RoomID(ref))
	default:
		portal, err = br.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(ref)})
	}
	if err != nil {
		return mergeRoom{}, fmt.Errorf("failed to look up %s: %w", ref, err)
	}
	if portal == nil {
		return mergeRoom{}, fmt.Errorf("no bridged room found for %s", ref)
	}
	room := mergeRoom{
		Input:  ref,
		Portal: portal,
	}
	room = mergeRoomFromPortalRow(portal.Portal, room)
	room.IsSpace = portal.RoomType == database.RoomTypeSpace || roomIsSpace(ctx, br, portal.MXID)
	return room, nil
}

// mergeRoomFromPortalRow fills a mergeRoom from a database portal row.
func mergeRoomFromPortalRow(row *database.Portal, room mergeRoom) mergeRoom {
	if row == nil {
		return room
	}
	room.RoomID = row.MXID
	room.PortalID = row.ID
	room.Name = strings.TrimSpace(row.Name)
	meta, _ := row.Metadata.(*PortalMetadata)
	room.Recipients = portalParticipantList(meta)
	if meta != nil {
		room.ThreadID = meta.ThreadID
		room.MergedInto = meta.MergedInto
		room.MergedFromCount = len(meta.MergedFrom)
	}
	if room.Name == "" && len(room.Recipients) > 0 {
		room.Name = PortalName("", room.Recipients)
	}
	return room
}

// roomIsSpace reports whether a Matrix room was created as a space. It reports
// false when the connector cannot inspect room state.
func roomIsSpace(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) bool {
	if roomID == "" || br == nil || br.Matrix == nil {
		return false
	}
	stateAPI, ok := br.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState)
	if !ok {
		return false
	}
	createEvent, err := stateAPI.GetStateEvent(ctx, roomID, event.StateCreate, "")
	if err != nil || createEvent == nil {
		return false
	}
	content := createEvent.Content.AsCreate()
	return content != nil && content.Type == event.RoomTypeSpace
}

// resolveRoomAlias resolves a #alias through the production Matrix connector.
// Test or alternate connectors without a bot intent cannot resolve aliases.
func resolveRoomAlias(ctx context.Context, br *bridgev2.Bridge, alias id.RoomAlias) (id.RoomID, error) {
	connector, ok := br.Matrix.(*matrix.Connector)
	if !ok || connector.Bot == nil {
		return "", fmt.Errorf("room alias resolution is not available; use a room ID or portal ID instead")
	}
	resp, err := connector.Bot.ResolveAlias(ctx, alias)
	if err != nil {
		return "", fmt.Errorf("failed to resolve room alias %s: %w", alias, err)
	}
	if resp == nil || resp.RoomID == "" {
		return "", fmt.Errorf("room alias %s does not resolve to a room", alias)
	}
	return resp.RoomID, nil
}

// commandPrefix returns the configured command prefix with a trailing space,
// with a safe default for bridges built without a config (tests).
func commandPrefix(br *bridgev2.Bridge) string {
	if br != nil && br.Config != nil && br.Config.CommandPrefix != "" {
		return br.Config.CommandPrefix + " "
	}
	return "!sms "
}

// applyMergePlanMetadata stores the merge registry in the metadata of the
// plan's portals: the target keeps the participant union and the MergedFrom
// list, every source its MergedInto pointer. Callers save the portals.
func applyMergePlanMetadata(plan *mergePlan) error {
	if plan == nil {
		return fmt.Errorf("merge plan is nil")
	}
	target := plan.Rooms[plan.Target]
	if target.Portal == nil {
		return fmt.Errorf("target portal is not loaded")
	}
	sourceIDs := make([]networkid.PortalID, 0, len(plan.Sources))
	for _, index := range plan.Sources {
		source := plan.Rooms[index]
		if source.Portal == nil {
			return fmt.Errorf("source portal %s is not loaded", source.RoomID)
		}
		sourceIDs = append(sourceIDs, source.PortalID)
	}
	targetMeta := portalMetadataOf(target.Portal)
	targetMeta.Recipients = plan.Participants
	targetMeta.IsGroup = len(plan.Participants) != 1
	if len(plan.Participants) == 1 {
		targetMeta.OtherUserID = MakeUserID(plan.Participants[0])
	} else {
		targetMeta.OtherUserID = ""
	}
	targetMeta.MergedFrom = mergeSortedIDs(append(targetMeta.MergedFrom, sourceIDs...))
	for _, index := range plan.Sources {
		portalMetadataOf(plan.Rooms[index].Portal).MergedInto = target.PortalID
	}
	return nil
}

// executeMerge applies a validated merge plan: registry metadata first, then
// the target room state and the tombstones.
func executeMerge(ctx context.Context, br *bridgev2.Bridge, user *bridgev2.User, login *bridgev2.UserLogin, plan *mergePlan) error {
	if br == nil || br.DB == nil {
		return fmt.Errorf("bridge is not available")
	}
	target := plan.Rooms[plan.Target]
	if target.Portal == nil {
		return fmt.Errorf("target portal is not loaded")
	}
	// Apply and persist the registry under the client's portal metadata lock so
	// a concurrently processing phone event cannot interleave its metadata.
	var client *MyNetworkClient
	if login != nil {
		client, _ = login.Client.(*MyNetworkClient)
	}
	err := func() error {
		if client != nil {
			client.portalMetaMu.Lock()
			defer client.portalMetaMu.Unlock()
		}
		if err := applyMergePlanMetadata(plan); err != nil {
			return err
		}
		if err := target.Portal.Save(ctx); err != nil {
			return fmt.Errorf("save target portal: %w", err)
		}
		for _, index := range plan.Sources {
			source := plan.Rooms[index]
			if err := source.Portal.Save(ctx); err != nil {
				return fmt.Errorf("save merged room %s: %w", source.RoomID, err)
			}
		}
		return nil
	}()
	if err != nil {
		return err
	}

	for _, index := range plan.Sources {
		source := plan.Rooms[index]
		if source.MergedInto != "" && source.MergedInto != target.PortalID {
			if err := removeMergedSource(ctx, br, source.MergedInto, source.PortalID); err != nil {
				br.Log.Warn().Err(err).
					Str("portal_id", string(source.PortalID)).
					Str("old_target", string(source.MergedInto)).
					Msg("Failed to remove re-pointed room from its old merge target")
			}
		}
		tombstone := &event.TombstoneEventContent{
			Body:            fmt.Sprintf("This conversation was merged into %s.", mergeEntryRoomLabel(target)),
			ReplacementRoom: target.RoomID,
		}
		if br.Bot != nil {
			if _, err := br.Bot.SendState(ctx, source.RoomID, event.StateTombstone, "", &event.Content{Parsed: tombstone}, time.Now()); err != nil {
				br.Log.Warn().Err(err).
					Str("room_id", string(source.RoomID)).
					Msg("Failed to send tombstone to merged room")
			}
			sendNotice(br.Bot, source.RoomID, fmt.Sprintf(
				"This room was merged into %s. New messages from the merged thread are bridged there; replies sent here still reach the conversation. Use `%sunmerge %s` to undo the routing.",
				mergeEntryRoomLabel(target), commandPrefix(br), source.RoomID))
		}
	}

	// The target is active, so a tombstone left from an earlier merge must go
	// and its topic must match the current merge metadata.
	if targetMeta, _ := target.Portal.Metadata.(*PortalMetadata); targetMeta != nil {
		clearMergeTombstone(ctx, br, target.RoomID)
		syncMergeTargetTopic(ctx, br, target.RoomID, targetMeta)
	}

	ownerLogin := mergeOwnerLogin(ctx, br, user, target.Portal.PortalKey)
	if ownerLogin == nil {
		ownerLogin = login
	}
	if ownerLogin != nil && ownerLogin.Client != nil {
		chatInfo := mergedChatInfo(ctx, br, plan.Participants, mergeTargetTopic(len(plan.Rooms), plan.Participants))
		br.QueueRemoteEvent(ownerLogin, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: target.Portal.PortalKey,
				Timestamp: time.Now(),
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: chatInfo},
		})
	}
	return nil
}

// removeMergedSource drops a source portal from a target's MergedFrom list.
// It works on the database row directly so it does not depend on the bridge's
// portal cache.
func removeMergedSource(ctx context.Context, br *bridgev2.Bridge, targetID, sourceID networkid.PortalID) error {
	if br == nil || br.DB == nil || targetID == "" || sourceID == "" {
		return nil
	}
	row, err := br.DB.Portal.GetByIDWithUncertainReceiver(ctx, networkid.PortalKey{ID: targetID})
	if err != nil || row == nil {
		return err
	}
	meta, _ := row.Metadata.(*PortalMetadata)
	if meta == nil || len(meta.MergedFrom) == 0 {
		return nil
	}
	meta.MergedFrom = mergeRemoveID(meta.MergedFrom, sourceID)
	return br.DB.Portal.Update(ctx, row)
}

// mergeOwnerLogin picks the user's login that owns the target portal, falling
// back to the command's default login.
func mergeOwnerLogin(ctx context.Context, br *bridgev2.Bridge, user *bridgev2.User, key networkid.PortalKey) *bridgev2.UserLogin {
	if br == nil || user == nil {
		return nil
	}
	logins, err := br.GetUserLoginsInPortal(ctx, key)
	if err != nil {
		return nil
	}
	for _, login := range logins {
		if login != nil && login.User != nil && login.User.MXID == user.MXID {
			return login
		}
	}
	return nil
}

// mergeEntry is one active merge for the `merges` listing.
type mergeEntry struct {
	Source mergeRoom
	Target mergeRoom
}

// mergedPortalsQuery selects portals that were merged into another portal.
// `metadata->>` matches the JSON access bridgev2 itself uses, so it works on
// both SQLite and PostgreSQL.
const mergedPortalsQuery = `
SELECT bridge_id, id, receiver, mxid, parent_id, parent_receiver, relay_login_id, other_user_id,
       name, topic, avatar_id, avatar_hash, avatar_mxc,
       name_set, topic_set, avatar_set, name_is_custom, in_space, message_request, user_blocked,
       room_type, disappear_type, disappear_timer, cap_state, metadata
FROM portal
WHERE bridge_id=$1 AND metadata->>'merged_into' IS NOT NULL AND metadata->>'merged_into' <> ''`

// listActiveMerges returns every portal that has MergedInto set, with its
// direct target.
func listActiveMerges(ctx context.Context, br *bridgev2.Bridge) ([]mergeEntry, error) {
	if br == nil || br.DB == nil {
		return nil, fmt.Errorf("bridge database is not available")
	}
	sources, err := br.DB.Portal.QueryMany(ctx, mergedPortalsQuery, br.DB.BridgeID)
	if err != nil {
		return nil, err
	}
	entries := make([]mergeEntry, 0, len(sources))
	for _, source := range sources {
		if source == nil {
			continue
		}
		meta, _ := source.Metadata.(*PortalMetadata)
		if meta == nil || meta.MergedInto == "" {
			continue
		}
		entry := mergeEntry{Source: mergeRoomFromPortalRow(source, mergeRoom{})}
		target, err := br.DB.Portal.GetByIDWithUncertainReceiver(ctx, networkid.PortalKey{ID: meta.MergedInto})
		if err != nil {
			return nil, err
		}
		if target != nil {
			entry.Target = mergeRoomFromPortalRow(target, mergeRoom{})
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.Target.RoomID != right.Target.RoomID {
			return left.Target.RoomID < right.Target.RoomID
		}
		return left.Source.RoomID < right.Source.RoomID
	})
	return entries, nil
}

// formatMergeEntries renders the `merges` command output.
func formatMergeEntries(entries []mergeEntry) string {
	if len(entries) == 0 {
		return "No active merges. Use `merge <room> <room>` to combine fractured conversations."
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := fmt.Sprintf("- %s → %s", mergeEntryRoomLabel(entry.Source), mergeEntryRoomLabel(entry.Target))
		if entry.Target.MergedInto != "" {
			line += fmt.Sprintf(" (the target is itself merged into %s)", entry.Target.MergedInto)
		}
		lines = append(lines, line)
	}
	return "Active merges (source → target):\n" + strings.Join(lines, "\n")
}

// CommandMerge combines fractured rooms into one target room. Future events for
// the merged-away threads are routed to the target.
var CommandMerge = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		req, err := parseMergeRequest(ce.Args)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login found")
			return
		}
		rooms := make([]mergeRoom, 0, len(req.Rooms)+1)
		for _, ref := range req.Rooms {
			room, err := resolveMergeRoom(ce.Ctx, ce.Bridge, ref)
			if err != nil {
				ce.Reply("%v", err)
				return
			}
			rooms = append(rooms, room)
		}
		target := 0
		if req.Into != "" {
			target = -1
			for i := range rooms {
				if mergeRoomMatchesRef(rooms[i], req.Into) {
					target = i
					break
				}
			}
			if target < 0 {
				room, err := resolveMergeRoom(ce.Ctx, ce.Bridge, req.Into)
				if err != nil {
					ce.Reply("%v", err)
					return
				}
				rooms = append(rooms, room)
				target = len(rooms) - 1
			}
		}
		for i := range rooms {
			rooms[i].IsManagementRoom = rooms[i].IsManagementRoom || rooms[i].RoomID == ce.User.ManagementRoom
		}
		plan, err := planMerge(rooms, target)
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		summary := formatMergePlanSummary(plan)
		if len(plan.Sources) == 0 {
			ce.Reply("Nothing to merge: every selected room is already merged into %s.\n\n%s",
				mergeEntryRoomLabel(plan.Rooms[plan.Target]), summary)
			return
		}
		if plan.Disconnected && !req.Yes {
			ce.Reply("Refusing to merge conversations with no shared participant.\n\n%s\n\nIf this is intentional, re-run with --yes.", summary)
			return
		}
		if plan.RequiresConfirmation() && !req.Yes {
			ce.Reply("Please review this merge before confirming:\n\n%s\n\nReasons: %s\n\nRe-run with --yes to proceed.",
				summary, strings.Join(plan.ConfirmationReasons, "; "))
			return
		}
		if err := executeMerge(ce.Ctx, ce.Bridge, ce.User, login, plan); err != nil {
			ce.Reply("Merge failed: %v", err)
			return
		}
		var rePointed []string
		for _, index := range plan.Sources {
			if plan.Rooms[index].MergedInto != "" {
				rePointed = append(rePointed, mergeRoomLabel(plan.Rooms[index], index))
			}
		}
		reply := fmt.Sprintf("Merged %d room(s) into %s. Future messages from the merged threads are bridged there.\n\n%s",
			len(plan.Sources), mergeEntryRoomLabel(plan.Rooms[plan.Target]), summary)
		if len(rePointed) > 0 {
			reply += fmt.Sprintf("\n\nRe-pointed rooms: %s", strings.Join(rePointed, ", "))
		}
		ce.Reply("%s", reply)
	},
	Name: "merge",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Merge rooms that belong to one phone conversation into a target room (usage: merge <room-id> <room-id> [...] [--into <room-id>] [--yes])",
		Args:        "<room-id> <room-id> [<room-id> ...] [--into <room-id>] [--yes]",
	},
	RequiresLogin: true,
	RequiresAdmin: true,
}

// CommandMerges lists active merges.
var CommandMerges = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		entries, err := listActiveMerges(ce.Ctx, ce.Bridge)
		if err != nil {
			ce.Reply("Failed to list merges: %v", err)
			return
		}
		ce.Reply("%s", formatMergeEntries(entries))
	},
	Name:    "merges",
	Aliases: []string{"list-merges"},
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "List rooms merged into another room",
	},
	RequiresLogin: true,
	RequiresAdmin: true,
}

// CommandUnmerge undoes the routing of one merged source. The source's tombstone
// is cleared and the target's topic is recomputed from the merge metadata.
var CommandUnmerge = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if len(ce.Args) != 1 {
			ce.Reply("Usage: %s", unmergeUsage)
			return
		}
		room, err := resolveMergeRoom(ce.Ctx, ce.Bridge, ce.Args[0])
		if err != nil {
			ce.Reply("%v", err)
			return
		}
		if room.Portal == nil {
			ce.Reply("Bridge portal for %s is not loaded", ce.Args[0])
			return
		}
		meta := portalMetadataOf(room.Portal)
		if meta.MergedInto == "" {
			ce.Reply("%s is not merged into another room.", mergeEntryRoomLabel(room))
			return
		}
		targetID := meta.MergedInto
		meta.MergedInto = ""
		if err := room.Portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save unmerged room: %v", err)
			return
		}
		if err := removeMergedSource(ce.Ctx, ce.Bridge, targetID, room.PortalID); err != nil {
			ce.Reply("Unmerged %s, but failed to update the target's merge list: %v", mergeEntryRoomLabel(room), err)
			return
		}
		clearMergeTombstone(ce.Ctx, ce.Bridge, room.RoomID)
		if targetRow, err := ce.Bridge.DB.Portal.GetByIDWithUncertainReceiver(ce.Ctx, networkid.PortalKey{ID: targetID}); err == nil && targetRow != nil && targetRow.MXID != "" {
			targetMeta, _ := targetRow.Metadata.(*PortalMetadata)
			syncMergeTargetTopic(ce.Ctx, ce.Bridge, targetRow.MXID, targetMeta)
		}
		ce.Reply("Unmerged %s. Its thread is bridged into its own room again; the merge tombstone was cleared and the target's topic updated.",
			mergeEntryRoomLabel(room))
	},
	Name: "unmerge",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Undo the merge routing of one room (usage: unmerge <room-id>)",
		Args:        "<room-id>",
	},
	RequiresLogin: true,
	RequiresAdmin: true,
}

// clearMergeTombstone removes a tombstone left on a room that is active again.
// Sending empty content clears the state.
func clearMergeTombstone(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID) {
	if br == nil || br.Bot == nil || roomID == "" {
		return
	}
	if _, err := br.Bot.SendState(ctx, roomID, event.StateTombstone, "", &event.Content{}, time.Now()); err != nil {
		br.Log.Warn().Err(err).Str("room_id", string(roomID)).Msg("Failed to clear merge tombstone")
	}
}

// mergeTopicForMeta renders the topic a room's merge metadata wants. An empty
// string means the room should not carry a merged-conversation topic.
func mergeTopicForMeta(meta *PortalMetadata) string {
	if meta == nil || len(meta.MergedFrom) == 0 {
		return ""
	}
	return mergeTargetTopic(len(meta.MergedFrom)+1, meta.Recipients)
}

// syncMergeTargetTopic pushes the topic implied by the merge metadata to the
// room, clearing a stale merged-conversation topic when none is due.
func syncMergeTargetTopic(ctx context.Context, br *bridgev2.Bridge, roomID id.RoomID, meta *PortalMetadata) {
	if br == nil || br.Bot == nil || roomID == "" {
		return
	}
	content := &event.Content{}
	if topic := mergeTopicForMeta(meta); topic != "" {
		content.Parsed = &event.TopicEventContent{Topic: topic}
	}
	if _, err := br.Bot.SendState(ctx, roomID, event.StateTopic, "", content, time.Now()); err != nil {
		br.Log.Warn().Err(err).Str("room_id", string(roomID)).Msg("Failed to update merged topic")
	}
}

// CommandMergeCleanup repairs merge artifacts left in room state: stale
// tombstones on rooms that are not merged away, and merged-conversation topics
// that no longer match the merge metadata.
var CommandMergeCleanup = &commands.FullHandler{
	Func: func(ce *commands.Event) {
		if ce.Bridge == nil || ce.Bridge.DB == nil {
			ce.Reply("The bridge database is not available.")
			return
		}
		stateAPI, ok := ce.Bridge.Matrix.(bridgev2.MatrixConnectorWithArbitraryRoomState)
		if !ok {
			ce.Reply("Room state access is not available.")
			return
		}
		login := ce.User.GetDefaultLogin()
		if login == nil {
			ce.Reply("No login to clean up.")
			return
		}
		userPortals, err := ce.Bridge.DB.UserPortal.GetAllForLogin(ce.Ctx, login.UserLogin)
		if err != nil {
			ce.Reply("Failed to list portals: %v", err)
			return
		}
		clearedTombstones := 0
		fixedTopics := 0
		for _, up := range userPortals {
			row, err := ce.Bridge.DB.Portal.GetByIDWithUncertainReceiver(ce.Ctx, up.Portal)
			if err != nil || row == nil || row.MXID == "" {
				continue
			}
			meta, _ := row.Metadata.(*PortalMetadata)
			if meta != nil && meta.MergedInto != "" {
				continue
			}
			if evt, err := stateAPI.GetStateEvent(ce.Ctx, row.MXID, event.StateTombstone, ""); err == nil && evt != nil {
				clearMergeTombstone(ce.Ctx, ce.Bridge, row.MXID)
				clearedTombstones++
			}
			want := mergeTopicForMeta(meta)
			if want == "" {
				if evt, err := stateAPI.GetStateEvent(ce.Ctx, row.MXID, event.StateTopic, ""); err == nil && evt != nil {
					if topic := evt.Content.AsTopic(); topic != nil && strings.HasPrefix(topic.Topic, "Merged phone conversation") {
						syncMergeTargetTopic(ce.Ctx, ce.Bridge, row.MXID, nil)
						fixedTopics++
					}
				}
			} else if row.Topic != want {
				syncMergeTargetTopic(ce.Ctx, ce.Bridge, row.MXID, meta)
				fixedTopics++
			}
		}
		ce.Reply("Merge cleanup done: cleared %d stale tombstone(s), fixed %d topic(s).", clearedTombstones, fixedTopics)
	},
	Name: "merge-cleanup",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionAdmin,
		Description: "Repair stale merge tombstones and merged-room topics",
	},
	RequiresLogin: true,
	RequiresAdmin: true,
}
