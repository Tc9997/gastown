package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// RigSummary holds aggregated stats for a single rig.
type RigSummary struct {
	Name            string
	PolecatsWorking int
	PolecatsTotal   int
	InMQ            int
	BeadsClosed     int
	BeadsTotal      int
}

// BeadCounts holds per-rig bead statistics.
type BeadCounts struct {
	Total  int
	Closed int
}

// FetchRigBeadCounts retrieves bead counts (total and closed) per rig.
func FetchRigBeadCounts(townRoot string) map[string]BeadCounts {
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil
	}

	counts := make(map[string]BeadCounts)
	for rigName := range rigsConfig.Rigs {
		rigPath := filepath.Join(townRoot, rigName)
		if _, err := os.Stat(rigPath); err != nil {
			continue
		}

		total := countBeads(rigPath, "")
		closed := countBeads(rigPath, "closed")
		if total > 0 {
			counts[rigName] = BeadCounts{Total: total, Closed: closed}
		}
	}

	return counts
}

// beadCountItem is a minimal struct for counting beads from bd list --json.
type beadCountItem struct {
	ID string `json:"id"`
}

// countBeads counts beads in a rig directory, optionally filtered by status.
func countBeads(rigPath, status string) int {
	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	args := []string{"list", "--type=task,bug,feature", "--json"}
	if status != "" {
		args = append(args, "--status="+status)
	}

	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = rigPath
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return 0
	}

	var items []beadCountItem
	if err := json.Unmarshal(stdout.Bytes(), &items); err != nil {
		return 0
	}

	return len(items)
}

// computeRigSummaries aggregates data from polecatState, convoyState, and
// beadCounts into per-rig summaries.
func (m *Model) computeRigSummaries() []RigSummary {
	summaries := make(map[string]*RigSummary)

	getSummary := func(name string) *RigSummary {
		s, ok := summaries[name]
		if !ok {
			s = &RigSummary{Name: name}
			summaries[name] = s
		}
		return s
	}

	// Aggregate polecat data
	if m.polecatState != nil {
		for _, p := range m.polecatState.Polecats {
			if p.Rig == "" {
				continue
			}
			s := getSummary(p.Rig)
			s.PolecatsTotal++
			if p.State == "working" {
				s.PolecatsWorking++
			}
		}
	}

	// Aggregate MQ entries
	if m.convoyState != nil {
		for _, entry := range m.convoyState.MQEntries {
			if entry.Rig == "" {
				continue
			}
			s := getSummary(entry.Rig)
			s.InMQ++
		}
	}

	// Aggregate bead counts
	for rig, bc := range m.rigBeadCounts {
		s := getSummary(rig)
		s.BeadsClosed = bc.Closed
		s.BeadsTotal = bc.Total
	}

	// Convert to sorted slice
	result := make([]RigSummary, 0, len(summaries))
	for _, s := range summaries {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result
}

// renderRigSummaryBar renders the one-line-per-rig summary bar.
// Caller must hold m.mu.
func (m *Model) renderRigSummaryBar() string {
	summaries := m.computeRigSummaries()
	if len(summaries) == 0 {
		return ""
	}

	var lines []string
	for _, s := range summaries {
		var parts []string

		// Polecats working
		if s.PolecatsTotal > 0 {
			parts = append(parts, fmt.Sprintf("%d polecats working", s.PolecatsWorking))
		}

		// MQ entries
		if s.InMQ > 0 {
			parts = append(parts, fmt.Sprintf("%d in MQ", s.InMQ))
		}

		// Beads closed
		if s.BeadsTotal > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d beads closed", s.BeadsClosed, s.BeadsTotal))
		}

		if len(parts) == 0 {
			continue
		}

		line := RigSummaryNameStyle.Render(s.Name+":") + " " +
			RigSummaryStatsStyle.Render(strings.Join(parts, " │ "))
		lines = append(lines, " "+line)
	}

	if len(lines) == 0 {
		return ""
	}

	return strings.Join(lines, "\n")
}
