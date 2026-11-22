package main

import (
	"errors"
	"fmt"

	"github.com/mirror-db/mirror-db/tools/relay/config"
	"github.com/mirror-db/mirror-db/tools/relay/hashstore"
	"github.com/spf13/cobra"
)

func pruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Delete cached content this relay does not index (offline)",
		Long: `Delete every cached object this relay's index does not know about.

This is an offline maintenance command: stop the relay first (the CACHE_DIR
bbolt lock refuses a second instance anyway). It opens the cache from CACHE_*
/ S3_* configuration and runs no server, edge selection, certificates or
manifest watcher.

WARNING: with S3_BUCKET configured this deletes from the SHARED cache. Every
object this relay does not index is removed, including objects written by other
relays and by relays that are online right now. Only run it against a bucket no
other relay depends on, or when you intend to drop everyone's cache. Deleted
content can be fetched upstream again; the upstream data is not affected.

The index is repaired first: rows whose body is gone and aliases whose primary
record is missing are removed, so pointers of an already-vanished body disappear
in the same run. A permission or I/O error keeps the row and fails the run.

Prints scanned/kept/deleted/pending/stale/skipped/failed counts to stdout and
exits non-zero if any object could not be checked, listed or deleted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrune(cmd)
		},
	}
}

func runPrune(cmd *cobra.Command) error {
	cfg, err := config.GetHashStoreConfig()
	if err != nil {
		return err
	}
	if cfg == nil {
		return errors.New("cache is not enabled: set CACHE_DIR and CACHE_CAPACITY before running `relay prune`")
	}

	store, err := hashstore.OpenOffline(*cfg)
	if err != nil {
		return fmt.Errorf("open cache dir %q (is the relay still running?): %w", cfg.Dir, err)
	}

	stats, pruneErr := store.Prune(cmd.Context())
	closeErr := store.Close()

	fmt.Fprintf(cmd.OutOrStdout(),
		"prune: scanned=%d kept=%d deleted=%d pending=%d stale=%d skipped=%d failed=%d\n",
		stats.Scanned, stats.Kept, stats.Deleted, stats.Pending, stats.Stale, stats.Skipped, stats.Failed)

	if pruneErr != nil {
		return fmt.Errorf("prune %s: %w", cfg.Dir, pruneErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close cache %s: %w", cfg.Dir, closeErr)
	}
	return nil
}
