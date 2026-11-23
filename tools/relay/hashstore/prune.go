package hashstore

import (
	"context"
	"errors"
	"fmt"
)

// PruneStats reports what an offline prune saw and did.
type PruneStats struct {
	// Scanned counts content objects seen in the backend (bodies and aliases).
	Scanned int
	// Kept counts objects this instance still indexes.
	Kept int
	// Deleted counts objects this prune removed from the backend.
	Deleted int
	// Pending counts queued deletes from earlier evictions that completed.
	Pending int
	// Stale counts index rows dropped: body records whose body was gone, plus
	// dangling alias rows whose primary record was missing.
	Stale int
	// Skipped counts listed keys under our prefix that are not content objects.
	// Other prefixes are never listed at all.
	Skipped int
	// Failed counts objects that could not be deleted or listed.
	Failed int
}

// Prune is the offline "delete what this relay does not index" pass. It finishes
// queued deletes, drops index entries whose body is gone, drops dangling alias
// rows, then walks the backend and deletes every body and digest pointer missing
// from this instance's index. The index cleanup runs first and for every backend,
// so a body another relay (or a human) removed also loses its pointers in the
// same pass.
//
// Shared storage makes that destructive: objects another relay wrote are deleted
// too, including objects of a relay that is online right now. That is the chosen
// semantic — the content is still fetchable upstream — but it is why this runs as
// a separate `relay prune` command and only while holding the bbolt lock.
//
// Normal reads and the startup reconcile never list shared storage; only Prune
// does. Prune never adopts objects, recomputes digests or reads body contents.
func (s *Store) Prune(ctx context.Context) (PruneStats, error) {
	var (
		stats    PruneStats
		failures []error
	)
	if !s.enter() {
		return stats, ErrClosed
	}
	defer s.leave()

	if err := ctx.Err(); err != nil {
		return stats, err
	}

	fail := func(err error) {
		stats.Failed++
		failures = append(failures, err)
	}

	// 1. Complete deletes queued by earlier evictions.
	s.mu.Lock()
	pending, err := s.index.pendingDeletes()
	s.mu.Unlock()
	if err != nil {
		return stats, err
	}
	for _, m := range pending {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return stats, errors.Join(failures...)
		}
		if err := s.backend.delete(ctx, m.SHA256, m.SHA1); err != nil {
			fail(fmt.Errorf("pending delete %s: %w", m.SHA256, err))
			continue
		}
		s.mu.Lock()
		err := s.index.clearPending(m.SHA256)
		s.mu.Unlock()
		if err != nil {
			fail(fmt.Errorf("clear pending %s: %w", m.SHA256, err))
			continue
		}
		stats.Pending++
	}

	// 2. Drop body records whose body is gone. This runs for every backend: local
	//    stats a file, shared storage HEADs the object. Offline, one HEAD per
	//    indexed body is acceptable. Only a definite absence drops the record;
	//    a transient or permission error keeps it and fails closed.
	s.mu.Lock()
	records, err := s.index.records()
	s.mu.Unlock()
	if err != nil {
		fail(fmt.Errorf("read index: %w", err))
	}
	for _, m := range records {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return stats, errors.Join(failures...)
		}
		if _, exists, statErr := s.backend.stat(ctx, m.SHA256); statErr != nil {
			fail(fmt.Errorf("check body %s: %w", m.SHA256, statErr))
			continue
		} else if exists {
			continue
		}
		s.mu.Lock()
		s.lru.Delete(m.SHA256)
		rmErr := s.index.remove(m)
		s.mu.Unlock()
		if rmErr != nil {
			fail(fmt.Errorf("drop stale %s: %w", m.SHA256, rmErr))
			continue
		}
		stats.Stale++
	}

	// 3. Drop alias rows whose primary record no longer exists. Normally
	//    index.remove keeps the two buckets in step, so this only repairs a
	//    historically damaged index — but after step 2 it also covers pointers
	//    left behind for bodies that are already gone.
	s.mu.Lock()
	dangling, err := s.index.pruneDanglingAliases()
	s.mu.Unlock()
	if err != nil {
		fail(fmt.Errorf("prune dangling aliases: %w", err))
	} else {
		stats.Stale += dangling
	}

	// 4. Drop variant metadata whose body record is gone. Normal eviction and
	//    deletion already do this in the transaction that removes the record;
	//    this repairs damage left by an older index or a manual edit. It touches
	//    only the local index, so it neither changes the counts above nor the
	//    backend scan below.
	s.mu.Lock()
	_, err = s.index.pruneDanglingVariants()
	s.mu.Unlock()
	if err != nil {
		fail(fmt.Errorf("prune dangling variants: %w", err))
	}

	// 5. Walk the backend and delete anything this index does not know about.
	err = s.backend.listObjects(ctx, func(ref objectRef) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ref.Kind == foreignObject {
			stats.Skipped++
			return nil
		}
		stats.Scanned++

		indexed, err := s.indexed(ref)
		if err != nil {
			fail(fmt.Errorf("check %s: %w", ref.describe(), err))
			return nil
		}
		if indexed {
			stats.Kept++
			return nil
		}
		if err := s.backend.deleteObject(ctx, ref); err != nil {
			fail(fmt.Errorf("delete %s: %w", ref.describe(), err))
			return nil // keep scanning: one bad object must not stop the pass
		}
		stats.Deleted++
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
		} else {
			fail(fmt.Errorf("list objects: %w", err))
		}
	}
	return stats, errors.Join(failures...)
}

// indexed reports whether an object is still part of this instance's index. A
// read error is returned rather than treated as "not indexed", so a broken index
// cannot turn into a bucket-wide delete.
func (s *Store) indexed(ref objectRef) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch ref.Kind {
	case bodyObject:
		_, found, err := s.index.get(ref.Digest)
		return found, err
	case sha1AliasObject:
		primary, found, err := s.index.lookupSHA1(ref.Digest)
		if err != nil || !found {
			return false, err
		}
		_, found, err = s.index.get(primary)
		return found, err
	case sha256AliasObject:
		_, found, err := s.index.get(ref.Digest)
		return found, err
	}
	return false, nil
}
