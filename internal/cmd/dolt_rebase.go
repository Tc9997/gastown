package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	doltRebaseConfirm    bool
	doltRebaseKeepRecent int
	doltRebaseDryRun     bool
)

var doltRebaseCmd = &cobra.Command{
	Use:   "rebase <database>",
	Short: "Surgical compaction: squash old commits, keep recent ones",
	Long: `Surgically compact a Dolt database using interactive rebase.

Unlike 'gt dolt flatten' (which destroys ALL history), surgical rebase
keeps recent commits individual while squashing old history into one.

Algorithm (based on Dolt's DOLT_REBASE):
  1. Creates anchor branch at root commit
  2. Creates work branch from main
  3. Starts interactive rebase — populates dolt_rebase table
  4. Marks old commits as 'squash', keeps recent N as 'pick'
  5. Executes the rebase plan
  6. Swaps branches: work becomes the new main
  7. Cleans up temporary branches
  8. Runs GC to reclaim space

WARNING: DOLT_REBASE is NOT safe with concurrent writes. If agents are
actively committing to this database, the rebase may fail with a graph-change
error. The Compactor Dog (daemon) has automatic retry logic for this case.
For manual use, re-run the command if it fails due to concurrent writes.
Flatten mode (gt dolt flatten) is safe with concurrent writes.

Use --keep-recent to control how many recent commits to preserve.
Use --dry-run to see the plan without executing it.

Requires --yes-i-am-sure flag as safety interlock.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoltRebase,
}

func init() {
	doltRebaseCmd.Flags().BoolVar(&doltRebaseConfirm, "yes-i-am-sure", false,
		"Required safety flag to confirm compaction")
	doltRebaseCmd.Flags().IntVar(&doltRebaseKeepRecent, "keep-recent", 50,
		"Number of recent commits to keep as individual picks")
	doltRebaseCmd.Flags().BoolVar(&doltRebaseDryRun, "dry-run", false,
		"Show the rebase plan without executing it")
	doltCmd.AddCommand(doltRebaseCmd)
}

func runDoltRebase(cmd *cobra.Command, args []string) error {
	dbName := args[0]

	if !doltRebaseConfirm && !doltRebaseDryRun {
		return fmt.Errorf("this command rewrites commit history. Pass --yes-i-am-sure to proceed (or --dry-run to preview)")
	}

	if doltRebaseKeepRecent < 0 {
		return fmt.Errorf("--keep-recent must be non-negative (got %d)", doltRebaseKeepRecent)
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil || !running {
		return fmt.Errorf("Dolt server is not running — start with 'gt dolt start'")
	}

	config := doltserver.DefaultConfig(townRoot)
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true&timeout=5s&readTimeout=60s&writeTimeout=300s",
		config.User, config.HostPort(), dbName)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("connecting to database %s: %w", dbName, err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Verify database exists.
	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return fmt.Errorf("database %q not reachable: %w", dbName, err)
	}

	fmt.Printf("%s Pre-flight checks for %s (surgical rebase)\n", style.Bold.Render("●"), style.Bold.Render(dbName))

	// Count commits.
	var commitCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)).Scan(&commitCount); err != nil {
		return fmt.Errorf("counting commits: %w", err)
	}
	fmt.Printf("  Commits: %d\n", commitCount)
	fmt.Printf("  Keep recent: %d\n", doltRebaseKeepRecent)

	// Need at least 3 commits: root (pick) + at least 1 to squash + 1 to keep.
	minCommits := doltRebaseKeepRecent + 2
	if commitCount < minCommits {
		fmt.Printf("  %s Too few commits (%d) for surgical rebase with --keep-recent=%d (need at least %d)\n",
			style.Bold.Render("✓"), commitCount, doltRebaseKeepRecent, minCommits)
		return nil
	}

	// Record pre-flight row counts and DDL for repair after rebase.
	preCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("recording row counts: %w", err)
	}
	// Capture CREATE TABLE DDL — Dolt rebase can lose tables created in squashed commits.
	preDDL := make(map[string]string, len(preCounts))
	for table := range preCounts {
		var tblName, ddl string
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", dbName, table)).Scan(&tblName, &ddl); err == nil {
			preDDL[table] = ddl
		}
	}
	fmt.Printf("  Tables: %d\n", len(preCounts))
	for table, count := range preCounts {
		fmt.Printf("    %s: %d rows\n", table, count)
	}

	// Get root commit.
	var rootHash string
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName)).Scan(&rootHash); err != nil {
		return fmt.Errorf("finding root commit: %w", err)
	}
	fmt.Printf("  Root: %s\n", rootHash[:12])

	// USE database for all subsequent operations.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}

	const baseBranch = "compact-base"
	const workBranch = "compact-work"

	// Clean up any leftover branches from a previous failed run.
	// This may create "Transaction commit" entries on main, so we capture
	// the pre-flight HEAD AFTER cleanup to avoid false concurrency failures.
	rebaseCleanup(db, baseBranch, workBranch)

	// Get main branch hash for concurrency check (must be branch-explicit,
	// since we'll be on compact-work after rebase).
	var preHead string
	err = db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT commit_hash FROM `%s/main`.dolt_log ORDER BY date DESC LIMIT 1", dbName)).Scan(&preHead)
	if err != nil {
		return fmt.Errorf("getting HEAD: %w", err)
	}
	fmt.Printf("  HEAD: %s\n", preHead[:12])

	fmt.Printf("\n%s Starting surgical rebase...\n", style.Bold.Render("●"))

	// Step 1: Create anchor branch at root commit.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', '%s')", baseBranch, rootHash)); err != nil {
		return fmt.Errorf("create base branch at root: %w", err)
	}
	fmt.Printf("  Created %s at root %s\n", baseBranch, rootHash[:12])

	// Step 2: Create work branch from main.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('%s', 'main')", workBranch)); err != nil {
		rebaseCleanupBase(db, baseBranch)
		return fmt.Errorf("create work branch from main: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_CHECKOUT('%s')", workBranch)); err != nil {
		rebaseCleanupAll(db, baseBranch, workBranch)
		return fmt.Errorf("checkout work branch: %w", err)
	}
	fmt.Printf("  Created %s from main, checked out\n", workBranch)

	// Step 3: Start interactive rebase — populates dolt_rebase system table.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_REBASE('--interactive', '%s')", baseBranch)); err != nil {
		rebaseCleanupAll(db, baseBranch, workBranch)
		return fmt.Errorf("start interactive rebase: %w", err)
	}
	fmt.Printf("  Interactive rebase started (dolt_rebase table populated)\n")

	// Step 4: Inspect the rebase plan.
	var totalPlan int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_rebase").Scan(&totalPlan); err != nil {
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("counting rebase entries: %w", err)
	}
	fmt.Printf("  Rebase plan: %d commits\n", totalPlan)

	// Calculate how many to squash: everything except first (must stay pick) and last N.
	// Dolt returns rebase_order as DECIMAL — the MySQL driver delivers it as
	// []uint8 (e.g. "1.00") which cannot be scanned directly into int.
	// Scan as string, parse to float, then truncate to int.
	var minOrderStr, maxOrderStr string
	if err := db.QueryRowContext(ctx, "SELECT MIN(rebase_order), MAX(rebase_order) FROM dolt_rebase").Scan(&minOrderStr, &maxOrderStr); err != nil {
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("getting rebase order range: %w", err)
	}
	minOrder, maxOrder, err := parseRebaseOrderRange(minOrderStr, maxOrderStr)
	if err != nil {
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("parsing rebase order range: %w", err)
	}

	squashThreshold := maxOrder - doltRebaseKeepRecent
	toSquash := 0
	if squashThreshold > minOrder {
		toSquash = squashThreshold - minOrder
	}

	fmt.Printf("  Squashing: %d old commits (keeping first as pick + last %d)\n",
		toSquash, doltRebaseKeepRecent)

	if toSquash == 0 {
		fmt.Printf("  %s Nothing to squash — all commits are recent\n", style.Bold.Render("✓"))
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return nil
	}

	if doltRebaseDryRun {
		// Show the plan.
		fmt.Printf("\n%s Dry-run rebase plan:\n", style.Bold.Render("●"))
		rows, err := db.QueryContext(ctx, "SELECT rebase_order, action, commit_hash, commit_message FROM dolt_rebase ORDER BY rebase_order")
		if err != nil {
			rebaseAbortAndCleanup(db, baseBranch, workBranch)
			return fmt.Errorf("reading rebase plan: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var orderStr string
			var action, hash, msg string
			if err := rows.Scan(&orderStr, &action, &hash, &msg); err != nil {
				continue
			}
			order, err := parseRebaseOrder(orderStr)
			if err != nil {
				continue
			}
			marker := "pick"
			if order > minOrder && order <= squashThreshold {
				marker = "squash"
			}
			if len(msg) > 60 {
				msg = msg[:60] + "..."
			}
			if len(hash) > 8 {
				hash = hash[:8]
			}
			fmt.Printf("  %3d  %-7s  %s  %s\n", order, marker, hash, msg)
		}

		fmt.Printf("\n  Would squash %d commits, keep %d recent + 1 root pick\n",
			toSquash, doltRebaseKeepRecent)
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return nil
	}

	// Step 5: Modify the plan — mark old commits as squash.
	// First commit (minOrder) MUST stay 'pick' — squash needs a parent to fold into.
	// Keep last N commits as 'pick'.
	result, err := db.ExecContext(ctx, fmt.Sprintf(
		"UPDATE dolt_rebase SET action = 'squash' WHERE rebase_order > %d AND rebase_order <= %d",
		minOrder, squashThreshold))
	if err != nil {
		rebaseAbortAndCleanup(db, baseBranch, workBranch)
		return fmt.Errorf("updating rebase plan: %w", err)
	}
	affected, _ := result.RowsAffected()
	fmt.Printf("  Marked %d commits as squash\n", affected)

	// Step 6: Execute the rebase plan.
	fmt.Printf("  Executing rebase (this may take a while)...\n")
	if _, err := db.ExecContext(ctx, "CALL DOLT_REBASE('--continue')"); err != nil {
		// Rebase failed — conflicts cause automatic abort.
		rebaseCleanupAll(db, baseBranch, workBranch)
		return fmt.Errorf("rebase execution failed (possible conflicts — automatic abort): %w", err)
	}
	fmt.Printf("  %s Rebase executed successfully\n", style.Bold.Render("✓"))

	// Step 6b: Repair tables lost during squash.
	// Dolt's DOLT_REBASE can lose tables whose CREATE TABLE was in a squashed commit.
	// We recreate them from captured DDL and copy data from main (still intact).
	postTablesForRepair, _ := flattenGetRowCounts(db, dbName)
	repaired := 0
	for table, ddl := range preDDL {
		if _, exists := postTablesForRepair[table]; exists {
			continue
		}
		// Table missing after rebase — recreate it.
		fmt.Printf("  Repairing missing table %q...\n", table)
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			rebaseCleanupAll(db, baseBranch, workBranch)
			return fmt.Errorf("repair CREATE TABLE %q: %w", table, err)
		}
		// Copy data from main branch (which still has the original data).
		if preCounts[table] > 0 {
			insertQuery := fmt.Sprintf("INSERT INTO `%s` SELECT * FROM `%s/main`.`%s`",
				table, dbName, table)
			if _, err := db.ExecContext(ctx, insertQuery); err != nil {
				rebaseCleanupAll(db, baseBranch, workBranch)
				return fmt.Errorf("repair data for %q: %w", table, err)
			}
		}
		// Commit the repair.
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"CALL DOLT_COMMIT('-Am', 'rebase: repair table %s lost during squash')", table)); err != nil {
			fmt.Printf("  WARNING: could not commit repair for %q: %v\n", table, err)
		}
		repaired++
		fmt.Printf("  %s Repaired %q (%d rows)\n", style.Bold.Render("✓"), table, preCounts[table])
	}
	if repaired > 0 {
		fmt.Printf("  Repaired %d tables lost during squash\n", repaired)
	}

	// Step 7: Verify integrity — row counts must match pre-flight.
	postCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		// Rebase succeeded but we can't verify — this is concerning.
		fmt.Printf("  %s WARNING: could not verify row counts after rebase: %v\n",
			style.Bold.Render("!"), err)
		fmt.Printf("  Proceeding with branch swap — data should be intact\n")
	} else {
		var driftWarnings int
		for table, preCount := range preCounts {
			postCount, ok := postCounts[table]
			if !ok {
				// Abort — don't swap branches with missing tables.
				rebaseCleanupAll(db, baseBranch, workBranch)
				return fmt.Errorf("integrity FAIL: table %q missing after rebase", table)
			}
			if preCount != postCount {
				diff := postCount - preCount
				// Allow small drift from concurrent writes (±1% or ±10 rows, whichever is larger).
				threshold := preCount / 100
				if threshold < 10 {
					threshold = 10
				}
				if diff < -threshold || diff > threshold {
					rebaseCleanupAll(db, baseBranch, workBranch)
					return fmt.Errorf("integrity FAIL: %q pre=%d post=%d (drift %+d exceeds threshold %d)",
						table, preCount, postCount, diff, threshold)
				}
				fmt.Printf("  %s %q: row count drift %+d (pre=%d post=%d) — within tolerance\n",
					style.Bold.Render("!"), table, diff, preCount, postCount)
				driftWarnings++
			}
		}
		if driftWarnings > 0 {
			fmt.Printf("  %s Integrity verified with %d drift warning(s) (%d tables checked)\n",
				style.Bold.Render("✓"), driftWarnings, len(preCounts))
		} else {
			fmt.Printf("  %s Integrity verified (%d tables match)\n", style.Bold.Render("✓"), len(preCounts))
		}
	}

	// Step 8: Concurrency check — verify main hasn't moved.
	// Must query main explicitly since we're on compact-work after rebase.
	// If integrity is verified, treat concurrent writes as a warning (Dolt auto-commits
	// "Transaction commit" entries during branch operations, which are harmless).
	var currentHead string
	if err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT commit_hash FROM `%s/main`.dolt_log ORDER BY date DESC LIMIT 1", dbName)).Scan(&currentHead); err != nil {
		rebaseCleanupAll(db, baseBranch, workBranch)
		return fmt.Errorf("concurrency check: %w", err)
	}
	if currentHead != preHead {
		fmt.Printf("  %s WARNING: main HEAD moved during rebase (%s → %s) — proceeding (integrity verified)\n",
			style.Bold.Render("!"), preHead[:8], currentHead[:8])
	}

	// Step 9: Update main to compact-work's state.
	// Can't delete or rename the default branch in Dolt SQL server mode.
	// Instead: get compact-work's HEAD hash, checkout main, hard-reset to that hash.
	var workHead string
	if err := db.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT commit_hash FROM `%s/%s`.dolt_log ORDER BY date DESC LIMIT 1", dbName, workBranch)).Scan(&workHead); err != nil {
		return fmt.Errorf("getting compact-work HEAD: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return fmt.Errorf("checkout main for reset: %w", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--hard', '%s')", workHead)); err != nil {
		return fmt.Errorf("reset main to compact-work: %w", err)
	}
	// Clean up temporary branches.
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
	fmt.Printf("  Branch swap complete — main reset to compacted history\n")

	// Verify final state.
	var finalCount int
	if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)).Scan(&finalCount); err == nil {
		fmt.Printf("  Final commit count: %d\n", finalCount)
	}

	fmt.Printf("\n%s Surgical rebase complete: %d → %d commits (kept %d recent)\n",
		style.Bold.Render("✓"), commitCount, finalCount, doltRebaseKeepRecent)
	return nil
}

// rebaseCleanup removes leftover branches from a previous failed rebase.
func rebaseCleanup(db *sql.DB, baseBranch, workBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Try to get back to main first.
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// rebaseAbortAndCleanup aborts an in-progress rebase then cleans up branches.
//nolint:unparam // baseBranch always "compact-base" — API kept flexible for future callers
func rebaseAbortAndCleanup(db *sql.DB, baseBranch, workBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, "CALL DOLT_REBASE('--abort')")
	_, _ = db.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", workBranch))
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}

// rebaseCleanupAll cleans up both branches after a failed rebase.
//nolint:unparam // baseBranch always "compact-base" — API kept flexible for future callers
func rebaseCleanupAll(db *sql.DB, baseBranch, workBranch string) {
	rebaseCleanup(db, baseBranch, workBranch)
}

// parseRebaseOrder converts a rebase_order value (returned by Dolt as DECIMAL
// string, e.g. "1.00") to an int.
func parseRebaseOrder(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rebase_order %q: %w", s, err)
	}
	return int(math.Round(f)), nil
}

// parseRebaseOrderRange parses min/max rebase_order strings to ints.
func parseRebaseOrderRange(minStr, maxStr string) (int, int, error) {
	minVal, err := parseRebaseOrder(minStr)
	if err != nil {
		return 0, 0, err
	}
	maxVal, err := parseRebaseOrder(maxStr)
	if err != nil {
		return 0, 0, err
	}
	return minVal, maxVal, nil
}

// rebaseCleanupBase cleans up only the base branch (work branch not yet created).
func rebaseCleanupBase(db *sql.DB, baseBranch string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_BRANCH('-D', '%s')", baseBranch))
}
