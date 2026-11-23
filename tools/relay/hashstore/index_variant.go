package hashstore

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"

	bolt "go.etcd.io/bbolt"
)

// countVariants counts every stored variant row, including a corrupt one. A
// corrupt row still occupies its slot until a write or prune removes it.
func countVariants(tx *bolt.Tx) (int, error) {
	n := 0
	err := tx.Bucket(variantsBucket).ForEach(func(_, _ []byte) error {
		n++
		return nil
	})
	return n, err
}

// variantTotal reads the transactional variant counter. A missing or damaged
// counter is repaired from the rows instead of failing: a counter problem must
// not become a body-eviction failure.
func variantTotal(tx *bolt.Tx) (int, error) {
	raw := tx.Bucket(variantMetaBucket).Get(variantCountKey)
	if len(raw) == 8 {
		return int(binary.BigEndian.Uint64(raw)), nil
	}
	return countVariants(tx)
}

func setVariantTotal(tx *bolt.Tx, n int) error {
	if n < 0 {
		n = 0
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	return tx.Bucket(variantMetaBucket).Put(variantCountKey, buf[:])
}

// variantURLCount counts the rows stored under one URL prefix. At most 16 rows
// are expected, so a prefix scan is cheaper than maintaining another counter
// that would also need repairing.
func variantURLCount(tx *bolt.Tx, url string) (int, error) {
	prefix := appendString(nil, url)
	cursor := tx.Bucket(variantsBucket).Cursor()
	n := 0
	for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
		n++
	}
	return n, nil
}

// checkVariantBodySize validates the declared body size against this instance's
// index. A shared body written by another relay has no local record, so the
// check is skipped; a local record must agree exactly, or a Range/revalidation
// path would later serve the wrong Content-Length.
func checkVariantBodySize(tx *bolt.Tx, v Variant) error {
	raw, err := rawKey(v.BodySHA256)
	if err != nil {
		return err
	}
	record := tx.Bucket(objectsBucket).Get(raw)
	if record == nil {
		return nil
	}
	meta, err := decodeRecord(raw, record)
	if err != nil {
		return fmt.Errorf("%w: body record for %s: %v", ErrInvalidVariant, v.BodySHA256, err)
	}
	if meta.Size != v.BodySize {
		return fmt.Errorf("%w: body %s is %d bytes, metadata says %d", ErrInvalidVariant, v.BodySHA256, meta.Size, v.BodySize)
	}
	return nil
}

// variants lists every readable variant whose key carries this URL as its
// prefix. The URL is length-prefixed in the key, so a shorter URL can never
// match a longer one that merely starts with the same bytes. A corrupt or
// unknown-version row is logged and skipped: one bad row must not hide all the
// others, and a caller can rewrite or delete it with the methods below.
func (ix *index) variants(url string) ([]Variant, error) {
	prefix := appendString(nil, url)
	var out []Variant
	err := ix.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(variantsBucket).Cursor()
		for k, v := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = cursor.Next() {
			decoded, err := decodeVariant(v)
			if err != nil {
				log.Printf("hashstore: skip corrupt variant for %q: %v", url, err)
				continue
			}
			expected, keyErr := variantKey(decoded)
			if keyErr != nil {
				log.Printf("hashstore: skip variant with an unusable identity for %q: %v", url, keyErr)
				continue
			}
			if !bytes.Equal(k, expected) {
				log.Printf("hashstore: skip variant whose key and value disagree for %q", url)
				continue
			}
			out = append(out, decoded)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// putVariant writes a variant and keeps its body reverse reference in step. A
// row with the same identity is replaced atomically; only a genuinely new
// identity is checked against the per-URL and global bounds. A corrupt row at
// the same key is dropped and rewritten; its stale references are left for
// prune, which reports rather than guesses.
func (ix *index) putVariant(v Variant) error {
	key, err := variantKey(v)
	if err != nil {
		return err
	}
	value, err := encodeVariant(v)
	if err != nil {
		return err
	}

	return ix.db.Update(func(tx *bolt.Tx) error {
		variants := tx.Bucket(variantsBucket)
		total, err := variantTotal(tx)
		if err != nil {
			return err
		}

		existing := false
		if previous := variants.Get(key); previous != nil {
			old, decodeErr := decodeVariant(previous)
			if decodeErr != nil {
				log.Printf("hashstore: replace corrupt variant: %v", decodeErr)
				if err := variants.Delete(key); err != nil {
					return err
				}
				total--
			} else {
				existing = true
				if err := deleteVariantRef(tx, key, old.BodySHA256); err != nil {
					return err
				}
			}
		}

		if !existing {
			if total >= maxVariantsGlobal {
				return fmt.Errorf("%w: %d of %d variants stored", ErrVariantLimit, total, maxVariantsGlobal)
			}
			count, err := variantURLCount(tx, v.URL)
			if err != nil {
				return err
			}
			if count >= maxVariantsPerURL {
				return fmt.Errorf("%w: URL has %d of %d variants", ErrVariantLimit, count, maxVariantsPerURL)
			}
		}

		if err := checkVariantBodySize(tx, v); err != nil {
			return err
		}
		if err := variants.Put(key, value); err != nil {
			return err
		}
		if err := putVariantRef(tx, key, v.BodySHA256); err != nil {
			return err
		}
		if !existing {
			total++
		}
		return setVariantTotal(tx, total)
	})
}

// replaceVariant swaps one identity for another atomically. It is used by
// revalidation, which may change the Vary set and therefore the key. Replacing
// an existing old row never grows the global count, so a full store still
// accepts an update; the per-URL bound is evaluated on the destination's
// resulting count. Corrupt old or destination rows are dropped and treated as
// absent.
func (ix *index) replaceVariant(old, next Variant) error {
	oldKey, err := variantKey(old)
	if err != nil {
		return err
	}
	newKey, err := variantKey(next)
	if err != nil {
		return err
	}
	value, err := encodeVariant(next)
	if err != nil {
		return err
	}
	sameKey := bytes.Equal(oldKey, newKey)

	return ix.db.Update(func(tx *bolt.Tx) error {
		variants := tx.Bucket(variantsBucket)
		total, err := variantTotal(tx)
		if err != nil {
			return err
		}

		var oldMeta Variant
		oldExists := false
		if row := variants.Get(oldKey); row != nil {
			decoded, decodeErr := decodeVariant(row)
			if decodeErr != nil {
				log.Printf("hashstore: drop corrupt variant being replaced: %v", decodeErr)
				if err := variants.Delete(oldKey); err != nil {
					return err
				}
				total--
			} else {
				oldMeta, oldExists = decoded, true
			}
		}

		var destMeta Variant
		destExists := false
		if !sameKey {
			if row := variants.Get(newKey); row != nil {
				decoded, decodeErr := decodeVariant(row)
				if decodeErr != nil {
					log.Printf("hashstore: drop corrupt variant at replacement destination: %v", decodeErr)
					if err := variants.Delete(newKey); err != nil {
						return err
					}
					total--
				} else {
					destMeta, destExists = decoded, true
				}
			}
		}

		// Bound only a row that is genuinely added at the destination.
		switch {
		case !sameKey && destExists:
			// The destination row is overwritten; nothing is added there.
		case sameKey && oldExists:
			// In-place update; nothing is added.
		default:
			count, err := variantURLCount(tx, next.URL)
			if err != nil {
				return err
			}
			result := count + 1
			if !sameKey && oldExists && oldMeta.URL == next.URL {
				// Old is in the same URL and is about to be removed.
				result = count
			}
			if result > maxVariantsPerURL {
				return fmt.Errorf("%w: URL has %d of %d variants", ErrVariantLimit, result, maxVariantsPerURL)
			}
			if !oldExists && total >= maxVariantsGlobal {
				return fmt.Errorf("%w: %d of %d variants stored", ErrVariantLimit, total, maxVariantsGlobal)
			}
		}

		if err := checkVariantBodySize(tx, next); err != nil {
			return err
		}

		delta := 0
		if sameKey {
			if oldExists {
				if err := deleteVariantRef(tx, oldKey, oldMeta.BodySHA256); err != nil {
					return err
				}
			} else {
				delta++
			}
		} else {
			if oldExists {
				if err := variants.Delete(oldKey); err != nil {
					return err
				}
				if err := deleteVariantRef(tx, oldKey, oldMeta.BodySHA256); err != nil {
					return err
				}
				delta--
			}
			if destExists {
				if err := deleteVariantRef(tx, newKey, destMeta.BodySHA256); err != nil {
					return err
				}
			} else {
				delta++
			}
		}

		if err := variants.Put(newKey, value); err != nil {
			return err
		}
		if err := putVariantRef(tx, newKey, next.BodySHA256); err != nil {
			return err
		}
		return setVariantTotal(tx, total+delta)
	})
}

// deleteVariant is idempotent: an absent row is not an error. A corrupt row is
// removed too; its body reference cannot be identified, so prune's dangling
// pass finishes the cleanup.
func (ix *index) deleteVariant(v Variant) error {
	key, err := variantKey(v)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		variants := tx.Bucket(variantsBucket)
		previous := variants.Get(key)
		if previous == nil {
			return nil
		}
		total, err := variantTotal(tx)
		if err != nil {
			return err
		}
		old, decodeErr := decodeVariant(previous)
		if decodeErr != nil {
			log.Printf("hashstore: delete corrupt variant: %v", decodeErr)
			if err := variants.Delete(key); err != nil {
				return err
			}
			return setVariantTotal(tx, total-1)
		}
		if err := variants.Delete(key); err != nil {
			return err
		}
		if err := deleteVariantRef(tx, key, old.BodySHA256); err != nil {
			return err
		}
		return setVariantTotal(tx, total-1)
	})
}

// putVariantRef records that a variant points at a body. The key is the raw
// 32-byte digest followed by the variant key, so one prefix scan finds every
// reference to a body.
func putVariantRef(tx *bolt.Tx, key []byte, sha256Hex string) error {
	ref, err := variantRefKey(key, sha256Hex)
	if err != nil {
		return err
	}
	return tx.Bucket(variantRefsBucket).Put(ref, []byte{1})
}

func deleteVariantRef(tx *bolt.Tx, key []byte, sha256Hex string) error {
	ref, err := variantRefKey(key, sha256Hex)
	if err != nil {
		return err
	}
	return tx.Bucket(variantRefsBucket).Delete(ref)
}

func variantRefKey(key []byte, sha256Hex string) ([]byte, error) {
	raw, err := rawKey(sha256Hex)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: variant body digest must be SHA-256", ErrInvalidDigest)
	}
	ref := make([]byte, 0, len(raw)+len(key))
	ref = append(ref, raw...)
	return append(ref, key...), nil
}

// removeVariantsForBody deletes every variant that references one body. It runs
// inside the transaction that removes the body record, so metadata can never
// outlive its body; the counter moves in the same transaction.
func (ix *index) removeVariantsForBody(tx *bolt.Tx, sha256Raw []byte) error {
	if len(sha256Raw) != 32 {
		return fmt.Errorf("%w: body digest must be 32 bytes", ErrInvalidDigest)
	}
	refs := tx.Bucket(variantRefsBucket)
	variants := tx.Bucket(variantsBucket)
	total, err := variantTotal(tx)
	if err != nil {
		return err
	}

	cursor := refs.Cursor()
	var references [][]byte
	for k, _ := cursor.Seek(sha256Raw); k != nil && bytes.HasPrefix(k, sha256Raw); k, _ = cursor.Next() {
		references = append(references, append([]byte(nil), k...))
	}
	removed := 0
	for _, ref := range references {
		key := ref[32:]
		if row := variants.Get(key); row != nil {
			decoded, decodeErr := decodeVariant(row)
			if decodeErr != nil {
				// The row is unusable; drop it and let prune report anything it
				// cannot interpret elsewhere.
				if err := variants.Delete(key); err != nil {
					return err
				}
				removed++
			} else if raw, err := rawKey(decoded.BodySHA256); err == nil && bytes.Equal(raw, sha256Raw) {
				if err := variants.Delete(key); err != nil {
					return err
				}
				removed++
			}
			// A reference whose variant now names a different body is stale:
			// remove only the reference and leave the live variant alone.
		}
		if err := refs.Delete(ref); err != nil {
			return err
		}
	}
	if removed > 0 {
		return setVariantTotal(tx, total-removed)
	}
	return nil
}

// removeVariantsForDigest deletes every variant that names a body, in its own
// transaction. Callers that remove a body outside remove/removeAndQueue (the
// local orphan repair) use it to keep metadata from outliving the file.
func (ix *index) removeVariantsForDigest(sha256Hex string) error {
	raw, err := rawKey(sha256Hex)
	if err != nil {
		return err
	}
	return ix.db.Update(func(tx *bolt.Tx) error {
		return ix.removeVariantsForBody(tx, raw)
	})
}

// pruneDanglingVariants drops variant rows whose body record is gone, then any
// reverse reference whose variant row is gone or names a different body. Normal
// removal keeps the buckets in step in one transaction; this only repairs
// historical damage or a crash, and never touches a backend object. A corrupt
// row fails the pass instead of being deleted blind: prune reports what it
// cannot understand, while the read and write paths self-heal.
func (ix *index) pruneDanglingVariants() (int, error) {
	removed := 0
	err := ix.db.Update(func(tx *bolt.Tx) error {
		variants := tx.Bucket(variantsBucket)
		objects := tx.Bucket(objectsBucket)
		refs := tx.Bucket(variantRefsBucket)
		total, err := variantTotal(tx)
		if err != nil {
			return err
		}

		var dangling [][]byte
		if err := variants.ForEach(func(k, v []byte) error {
			decoded, err := decodeVariant(v)
			if err != nil {
				return fmt.Errorf("decode variant: %w", err)
			}
			expected, keyErr := variantKey(decoded)
			if keyErr != nil || !bytes.Equal(k, expected) {
				// The row's canonical identity does not match the key it is stored
				// under; no listing or delete by identity can reach it.
				dangling = append(dangling, append([]byte(nil), k...))
				return nil
			}
			raw, err := rawKey(decoded.BodySHA256)
			if err != nil {
				return err
			}
			if len(raw) != 32 {
				return fmt.Errorf("%w: variant body digest must be SHA-256", ErrInvalidDigest)
			}
			if objects.Get(raw) == nil {
				dangling = append(dangling, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range dangling {
			if err := variants.Delete(key); err != nil {
				return err
			}
			removed++
		}
		if removed > 0 {
			if err := setVariantTotal(tx, total-removed); err != nil {
				return err
			}
		}

		var staleRefs [][]byte
		if err := refs.ForEach(func(k, _ []byte) error {
			if len(k) <= 32 {
				return nil
			}
			value := variants.Get(k[32:])
			if value == nil {
				staleRefs = append(staleRefs, append([]byte(nil), k...))
				return nil
			}
			decoded, err := decodeVariant(value)
			if err != nil {
				return fmt.Errorf("decode variant: %w", err)
			}
			raw, err := rawKey(decoded.BodySHA256)
			if err != nil {
				return err
			}
			if !bytes.Equal(k[:32], raw) {
				staleRefs = append(staleRefs, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, ref := range staleRefs {
			if err := refs.Delete(ref); err != nil {
				return err
			}
		}

		// Restore the reference each surviving variant should have. A stale
		// reference above may have been the only one, and without it eviction
		// could not find the metadata.
		if err := variants.ForEach(func(k, v []byte) error {
			decoded, err := decodeVariant(v)
			if err != nil {
				return fmt.Errorf("decode variant: %w", err)
			}
			ref, err := variantRefKey(k, decoded.BodySHA256)
			if err != nil {
				return err
			}
			if refs.Get(ref) != nil {
				return nil
			}
			return refs.Put(ref, []byte{1})
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
