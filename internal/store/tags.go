package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// NormaliseTags cleans operator input into a canonical, de-duplicated set.
// Tags are matched exactly when targeting, so "Web", "web " and "web" must not
// become three different groups.
func NormaliseTags(tags []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		// Commas are the storage separator, so they cannot appear in a tag.
		t = strings.ReplaceAll(t, ",", "")
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// SetDeviceTags replaces a device's labels.
func (s *Store) SetDeviceTags(ctx context.Context, id string, tags []string) ([]string, error) {
	clean := NormaliseTags(tags)
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET tags = ? WHERE id = ?`, strings.Join(clean, ","), id)
	if err != nil {
		return nil, fmt.Errorf("set device tags: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return clean, nil
}

// TagCount is one label and how many devices carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// ListTags returns every tag in use, most common first. Retired devices are
// excluded: they are history, not targets.
func (s *Store) ListTags(ctx context.Context) ([]TagCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tags FROM devices WHERE tags != '' AND status != ?`, StatusRetired)
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var tags string
		if err := rows.Scan(&tags); err != nil {
			return nil, fmt.Errorf("scan tags: %w", err)
		}
		for _, t := range strings.Split(tags, ",") {
			if t != "" {
				counts[t]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]TagCount, 0, len(counts))
	for tag, n := range counts {
		out = append(out, TagCount{Tag: tag, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out, nil
}

// Skipped explains why a device was left out of a fleet dispatch.
type Skipped struct {
	DeviceID string `json:"device_id"`
	Hostname string `json:"hostname"`
	Reason   string `json:"reason"`
}

// DispatchMany queues a script against many devices, skipping those it cannot
// run on instead of failing the whole request.
//
// This differs deliberately from Dispatch. Naming devices explicitly is a
// precise instruction, so an incompatible one is an error worth stopping for.
// Targeting a group is a broad instruction across a mixed fleet, where skipping
// a Windows box for a shell script is expected — but it must still be reported,
// or an operator would believe a fleet-wide run covered everything.
func (s *Store) DispatchMany(ctx context.Context, scriptID string, deviceIDs []string, createdBy string) ([]Job, []Skipped, error) {
	if len(deviceIDs) == 0 {
		return nil, nil, fmt.Errorf("select at least one device")
	}
	script, err := s.GetScript(ctx, scriptID)
	if err != nil {
		return nil, nil, err
	}

	var (
		eligible []string
		skipped  []Skipped
	)
	for _, id := range deviceIDs {
		d, err := s.GetDevice(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case d.RetiredAt != nil:
			skipped = append(skipped, Skipped{d.ID, d.Hostname, "being retired"})
		case !InterpreterSupports(script.Interpreter, d.OS):
			skipped = append(skipped, Skipped{d.ID, d.Hostname,
				fmt.Sprintf("runs %s, cannot execute %s", d.OS, script.Interpreter)})
		default:
			eligible = append(eligible, d.ID)
		}
	}

	if len(eligible) == 0 {
		return nil, skipped, fmt.Errorf("no selected device can run a %s script", script.Interpreter)
	}

	jobs, err := s.Dispatch(ctx, scriptID, eligible, createdBy)
	if err != nil {
		return nil, skipped, err
	}
	return jobs, skipped, nil
}
