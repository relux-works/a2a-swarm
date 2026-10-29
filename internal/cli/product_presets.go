package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type presetRoom struct{ id, label string }
type presetSub struct {
	room            string
	kinds, subtypes []string
}

var productPresets = []string{"orch", "keeper", "meta-keeper", "arbiter", "service"}

func validProductPreset(p string) bool { return contains(productPresets, p) }

// productScoped presets belong to one product and need --product.
func productScoped(p string) bool { return p == "orch" || p == "keeper" || p == "service" }

// parsePresets validates a comma-separated --preset list: one identity may
// hold several roles (e.g. a product keeper that is also the meta-keeper).
// An empty value means no preset.
func parsePresets(value, product, resource string) ([]string, error) {
	if value == "" {
		if resource != "" {
			return nil, usagef("--resource is only valid with --preset arbiter")
		}
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(value, ",") {
		p = strings.TrimSpace(p)
		if !validProductPreset(p) {
			return nil, usagef("unknown preset %q (one of: %s)", sanitizeLine(p), strings.Join(productPresets, ", "))
		}
		if contains(out, p) {
			return nil, usagef("preset %q given twice", p)
		}
		if productScoped(p) && product == "" {
			return nil, usagef("--preset %s requires --product", p)
		}
		out = append(out, p)
	}
	if contains(out, "arbiter") != (resource != "") {
		return nil, usagef("--preset arbiter and --resource ROOM go together")
	}
	return out, nil
}

// presetTag marks the subscriptions a preset owns, scoped so that two rows of
// one preset (a keeper of two products) never reconcile each other away.
func presetTag(preset, product, resource string) string {
	switch {
	case productScoped(preset):
		return preset + ":" + product
	case preset == "arbiter":
		return preset + ":" + resource
	}
	return preset
}

func (a *App) onboardPreset(ctx context.Context, s *session, presets []string, product, project, resource, dir string, rt hookRuntime, rtSource string, owners ownerPlan) error {
	var rooms []presetRoom
	plans := map[string][]presetSub{}
	for _, preset := range presets {
		r, sels, err := productPresetPlan(ctx, s, preset, product, project, resource, true)
		if err != nil {
			return err
		}
		rooms = append(rooms, r...)
		plans[preset] = sels
	}
	rooms = uniquePresetRooms(rooms)
	joined, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	in := map[string]bool{}
	for _, id := range joined {
		in[id] = true
	}
	a.printf("rooms:\n")
	for _, r := range rooms {
		if in[r.id] {
			a.printf("already in %s %s\n", r.label, r.id)
		} else {
			if _, err := s.client.Join(ctx, r.id); err != nil {
				return fmt.Errorf("join %s: %w", r.label, err)
			}
			in[r.id] = true
			a.printf("joined %s %s\n", r.label, r.id)
		}
		// Services have no session. Every session preset gets the ordinary
		// room baseline in addition to its role-specific selectors.
		if len(presets) != 1 || presets[0] != "service" {
			if err := s.subscribeRoom(ctx, a, r.id, r.label, a.Now()); err != nil {
				return err
			}
		}
	}
	for _, preset := range presets {
		if preset == "service" {
			a.printf("subscriptions (%s): none (service preset)\n", preset)
			continue
		}
		tag := presetTag(preset, product, resource)
		a.printf("subscriptions (%s):\n", tag)
		if err := s.reconcilePresetSubs(ctx, tag, plans[preset], a.Now()); err != nil {
			return err
		}
	}
	if owners.applies {
		if err := s.ensureOwnerSubs(ctx, a, owners); err != nil {
			return err
		}
	}
	if rt.name == "none" {
		a.printf("hooks: none (--runtime none: a service account, no session to notify)\n")
		return nil
	}
	a.printf("hooks (%s, %s):\n", rt.name, rtSource)
	settings := filepath.Join(dir, rt.settings)
	if abs, err := filepath.Abs(settings); err == nil {
		settings = abs
	}
	if err := cmdHookInstall(a, []string{"--agent", s.name, "--runtime", rt.name, "--settings", settings}, true); err != nil {
		return err
	}
	a.printWakeGuidance(rt.name)
	return nil
}

func productPresetPlan(ctx context.Context, s *session, preset, product, project, resource string, joinSpace bool) ([]presetRoom, []presetSub, error) {
	// A project room inside product X is #X-<project> (docs/products.md), so the
	// identity keeps its git-derived project while the room carries the product.
	projectRoom := "#" + product + "-" + project
	var aliases []string
	switch preset {
	case "orch":
		aliases = []string{"#lobby", "#broadcast", "#" + product + "-lobby", "#" + product + "-broadcast", projectRoom}
	case "keeper":
		aliases = []string{"#lobby", "#broadcast", "#" + product + "-lobby", "#" + product + "-broadcast", "#" + product + "-system"}
	case "meta-keeper":
		aliases = []string{"#lobby", "#broadcast", "#system"}
	case "service":
		aliases = []string{"#lobby", "#broadcast", "#" + product + "-lobby", "#" + product + "-broadcast"}
	case "arbiter":
		aliases = []string{resource}
	}
	rooms := []presetRoom{}
	addAlias := func(alias string) error {
		id, label, err := s.resolveRoom(ctx, alias)
		if err != nil {
			return fmt.Errorf("required room %s: %w", alias, err)
		}
		rooms = append(rooms, presetRoom{id, label})
		return nil
	}
	for _, alias := range aliases {
		if err := addAlias(alias); err != nil {
			return nil, nil, err
		}
	}
	if preset == "keeper" {
		space, label, err := s.resolveRoom(ctx, "#"+product)
		if err != nil {
			return nil, nil, err
		}
		joined, err := s.client.JoinedRooms(ctx)
		if err != nil {
			return nil, nil, err
		}
		inSpace := false
		for _, id := range joined {
			if id == space {
				inSpace = true
				break
			}
		}
		if !inSpace {
			if !joinSpace {
				return nil, nil, fmt.Errorf("join product space %s before syncing its children", label)
			}
			if _, err := s.client.Join(ctx, space); err != nil {
				return nil, nil, fmt.Errorf("join product space %s: %w", label, err)
			}
		}
		if _, _, err := s.productSpace(ctx, product); err != nil {
			return nil, nil, err
		}
		rooms = append(rooms, presetRoom{space, label})
		children, err := s.spaceChildren(ctx, space)
		if err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		for _, r := range rooms {
			seen[r.id] = true
		}
		for _, id := range children {
			if !seen[id] {
				rooms = append(rooms, presetRoom{id, s.roomLabel(ctx, id, nil)})
				seen[id] = true
			}
		}
	}
	if preset == "meta-keeper" {
		joined, err := s.client.JoinedRooms(ctx)
		if err != nil {
			return nil, nil, err
		}
		seen := map[string]bool{}
		for _, r := range rooms {
			seen[r.id] = true
		}
		for _, id := range joined {
			var c map[string]any
			if s.client.GetState(ctx, id, "m.room.create", "", &c) != nil || c["type"] != "m.space" {
				continue
			}
			children, err := s.spaceChildren(ctx, id)
			if err != nil {
				continue
			}
			productLabel := s.roomLabel(ctx, id, nil)
			lobbyLabel := productLabel + "-lobby"
			lobbyID, _, err := s.resolveRoom(ctx, lobbyLabel)
			if err != nil || seen[lobbyID] {
				continue
			}
			linked := false
			for _, child := range children {
				if child == lobbyID {
					linked = true
					break
				}
			}
			if linked {
				rooms = append(rooms, presetRoom{lobbyID, lobbyLabel})
				seen[lobbyID] = true
			}
		}
	}
	if preset == "arbiter" { // only the requested resource room
		rooms = rooms[:1]
	}
	var sels []presetSub
	roomID := func(label string) string {
		for _, r := range rooms {
			if r.label == label {
				return r.id
			}
		}
		return ""
	}
	add := func(label string, kinds, subtypes []string) {
		sels = append(sels, presetSub{roomID(label), kinds, subtypes})
	}
	switch preset {
	case "orch":
		add(projectRoom, []string{"system"}, []string{"incident", "blocker", "budget", "impact-start", "impact-end"})
		add(projectRoom, []string{"decision"}, nil)
		add("#"+product+"-lobby", []string{"decision", "question"}, nil)
		add("#lobby", []string{"decision"}, nil)
	case "keeper":
		add("#"+product+"-system", []string{"owner-needed"}, nil)
		add("#"+product+"-system", []string{"system"}, []string{"incident", "blocker", "budget", "window", "queue", "impact-start", "impact-end"})
		add("#"+product+"-lobby", []string{"decision"}, nil)
		add("#lobby", []string{"decision"}, nil)
	case "meta-keeper":
		add("#system", []string{"owner-needed"}, nil)
		add("#system", []string{"system"}, []string{"incident", "blocker", "budget", "impact-start", "impact-end"})
		add("#lobby", []string{"decision"}, nil)
	case "arbiter":
		add(rooms[0].label, []string{"system"}, []string{"window", "landing", "incident", "blocker", "queue"})
		add(rooms[0].label, []string{"decision"}, nil)
	}
	return uniquePresetRooms(rooms), sels, nil
}

func uniquePresetRooms(in []presetRoom) []presetRoom {
	out := in[:0]
	seen := map[string]bool{}
	for _, r := range in {
		if !seen[r.id] {
			out = append(out, r)
			seen[r.id] = true
		}
	}
	return out
}

// reconcilePresetSubs makes the subscriptions tagged tag exactly sels. It never
// touches hand-made subscriptions or those of another tag.
func (s *session) reconcilePresetSubs(ctx context.Context, tag string, sels []presetSub, now time.Time) error {
	return config.WithRuntimeLock(s.paths, s.name, "subscription-policy", time.Now().Add(nativeRPCTimeout), func() error {
		return s.reconcilePresetSubsWithPolicy(ctx, tag, sels, now)
	})
}

func (s *session) reconcilePresetSubsWithPolicy(ctx context.Context, tag string, sels []presetSub, now time.Time) error {
	policy, err := s.subscriptionPolicy()
	if err != nil {
		return err
	}
	desired := make([]Sub, 0, len(sels))
	for _, x := range sels {
		kinds := withoutTrim(x.kinds, policy.Trims[x.room])
		if len(kinds) == 0 {
			continue
		}
		sel := SubSelector{Room: x.room, Kinds: kinds, Subtypes: x.subtypes}
		if err := sel.validate(); err != nil {
			return err
		}
		desired = append(desired, Sub{SubSelector: sel, Preset: tag})
	}
	var messages []string
	_, _, err = s.updateSubsWithPolicy(ctx, policy, func(_ *config.State, list *SubList) (bool, error) {
		changed := false
		keep := list.Subs[:0]
		kept := make([]bool, len(desired))
		for _, cur := range list.Subs {
			if cur.Preset == tag {
				match := -1
				for i, want := range desired {
					if cur.SubSelector.equal(want.SubSelector) {
						match = i
						break
					}
				}
				if match < 0 || kept[match] {
					messages = append(messages, "removed "+cur.ID)
					changed = true
					continue
				}
				kept[match] = true
			}
			keep = append(keep, cur)
		}
		list.Subs = keep
		for i, want := range desired {
			if kept[i] {
				continue
			}
			want.ID = newSubID(*list)
			want.CreatedAt = now.UTC().Truncate(time.Millisecond).Format(createdAtLayout)
			list.Subs = append(list.Subs, want)
			messages = append(messages, "added "+want.ID)
			changed = true
		}
		return changed, nil
	})
	if err != nil {
		return err
	}
	for _, line := range messages {
		s.app.printf("%s\n", line)
	}
	if len(messages) == 0 {
		s.app.printf("unchanged\n")
	}
	return nil
}

func cmdSubsPresetSync(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("subs sync")
	preset := fs.String("preset", "", "preset to reconcile")
	product := fs.String("product", "", "product name")
	resource := fs.String("resource", "", "arbiter resource room")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a subs sync [--preset P --product X] [--resource ROOM]")
	}
	if *preset == "" {
		return usagef("subs sync --preset P[,P...] [--product X] [--resource ROOM]")
	}
	if *product != "" && !productNameRE.MatchString(*product) {
		return usagef("invalid product name %q: use [a-z0-9-]{1,40}", sanitizeLine(*product))
	}
	presets, err := parsePresets(*preset, *product, *resource)
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	project := ""
	parts := strings.Split(s.name, "_")
	if len(parts) >= 3 {
		project = strings.Join(parts[1:len(parts)-1], "_")
	}
	if contains(presets, "orch") && project == "" {
		return fmt.Errorf("cannot determine project from identity %s", s.name)
	}
	sole := len(presets) == 1 && presets[0] == "service"
	var owners ownerPlan
	if !sole {
		if owners, err = resolveOwnerPlan(s.paths, nil); err != nil {
			return err
		}
	}
	for _, p := range presets {
		if p == "service" {
			continue
		}
		_, sels, err := productPresetPlan(ctx, s, p, *product, project, *resource, false)
		if err != nil {
			return err
		}
		tag := presetTag(p, *product, *resource)
		a.printf("subscriptions (%s):\n", tag)
		if err := s.reconcilePresetSubs(ctx, tag, sels, a.Now()); err != nil {
			return err
		}
	}
	if sole {
		return nil
	}
	return s.ensureOwnerSubs(ctx, a, owners)
}
