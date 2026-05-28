// fix-meta.go
//
// One-shot tool to remove pv-snapshotter snapshot references from
// containerd's main metadata BoltDB without losing other state.
//
// Drops the bucket  v1/<namespace>/snapshots/pv-snapshotter
// in every namespace.  All other buckets (containers, images, leases,
// content) are preserved unchanged.
//
// SAFETY:
//   - Defaults to --dry-run.  Pass --apply to actually modify the database.
//   - Always opens the DB read-only first to log a complete plan, so the
//     intended changes can be audited before --apply is used.
//   - In --apply mode, makes a sibling backup file <db>.fix-meta-backup-<ts>
//     and verifies the backup byte-by-byte (SHA-256) before performing any
//     write.
//   - The write itself happens inside a single BoltDB transaction; bbolt
//     guarantees atomicity, so if the process is killed mid-transaction
//     the file is rolled back to its pre-transaction state.
//
// Usage:
//   go build -o fix-meta main.go
//   systemctl stop containerd
//   ./fix-meta /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db          # dry-run
//   ./fix-meta /var/lib/containerd/io.containerd.metadata.v1.bolt/meta.db --apply  # commit
//   systemctl start containerd
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	pvSnapshotter   = "pv-snapshotter"
	bucketV1        = "v1"
	bucketSnapshots = "snapshots"
)

func main() {
	args := os.Args[1:]
	apply := false
	var dbPath string
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		case "-h", "--help":
			usage()
			return
		default:
			if dbPath != "" {
				fmt.Fprintln(os.Stderr, "ERROR: unexpected positional argument:", a)
				usage()
				os.Exit(2)
			}
			dbPath = a
		}
	}
	if dbPath == "" {
		usage()
		os.Exit(2)
	}

	logf("=== fix-meta starting ===")
	logf("db path     : %s", dbPath)
	logf("apply mode  : %v", apply)
	if !apply {
		logf("(dry-run — no changes will be written; pass --apply to commit)")
	}

	st, err := os.Stat(dbPath)
	if err != nil {
		fatalf("cannot stat %s: %v", dbPath, err)
	}
	logf("db size     : %d bytes (mtime %s)", st.Size(), st.ModTime().Format(time.RFC3339))

	// ── Phase 1: open read-only, build the plan ─────────────────────────────
	logf("--- Phase 1: reading current state (read-only) ---")
	plan, err := readPlan(dbPath)
	if err != nil {
		fatalf("phase 1 failed: %v", err)
	}
	if len(plan.namespaces) == 0 {
		logf("no namespaces found under %s/v1; nothing to do.", dbPath)
		return
	}
	totalEntries := 0
	totalNS := 0
	for _, e := range plan.namespaces {
		if e.hasPVBucket {
			totalNS++
			totalEntries += e.entryCount
			logf("  namespace=%-20q pv-snapshotter bucket: %d entries  -> WILL DROP", e.name, e.entryCount)
		} else {
			logf("  namespace=%-20q pv-snapshotter bucket: not present (skip)", e.name)
		}
	}
	logf("plan summary: drop pv-snapshotter bucket in %d namespace(s), %d entries total", totalNS, totalEntries)

	if totalNS == 0 {
		logf("nothing to drop; all namespaces are already clean.")
		return
	}

	if !apply {
		logf("--- Phase 2: SKIPPED (dry-run).  Re-run with --apply to commit. ---")
		return
	}

	// ── Phase 2: backup ─────────────────────────────────────────────────────
	logf("--- Phase 2: creating verified backup ---")
	backupPath := fmt.Sprintf("%s.fix-meta-backup-%d", dbPath, time.Now().Unix())
	if err := safeBackup(dbPath, backupPath); err != nil {
		fatalf("backup failed: %v", err)
	}
	logf("backup written and verified: %s", backupPath)

	// ── Phase 3: write ──────────────────────────────────────────────────────
	logf("--- Phase 3: applying drop in single BoltDB transaction ---")
	dropped, err := dropPVBuckets(dbPath)
	if err != nil {
		logf("ERROR during drop: %v", err)
		logf("BoltDB transaction is atomic — the database has been rolled back.")
		logf("Backup remains at: %s (no manual restore needed)", backupPath)
		os.Exit(1)
	}
	logf("dropped %d bucket(s) successfully:", len(dropped))
	for _, d := range dropped {
		logf("  namespace=%q entries=%d", d.namespace, d.entries)
	}

	// ── Phase 4: post-write verification ────────────────────────────────────
	logf("--- Phase 4: verifying post-write state ---")
	postPlan, err := readPlan(dbPath)
	if err != nil {
		logf("WARNING: post-write read failed: %v", err)
		logf("Backup at %s can be restored if containerd fails to start.", backupPath)
	} else {
		stillPresent := 0
		for _, e := range postPlan.namespaces {
			if e.hasPVBucket {
				stillPresent++
				logf("  WARNING: namespace=%q still has pv-snapshotter bucket (%d entries)", e.name, e.entryCount)
			}
		}
		if stillPresent == 0 {
			logf("verified: no pv-snapshotter buckets remain in any namespace.")
		}
	}

	logf("=== fix-meta done ===")
	logf("If containerd fails to start, restore with:")
	logf("  systemctl stop containerd && cp %s %s && systemctl start containerd", backupPath, dbPath)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: fix-meta <path-to-meta.db> [--apply]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Without --apply, runs in dry-run mode and only logs the plan.")
	fmt.Fprintln(os.Stderr, "With --apply, makes a verified backup, then drops the")
	fmt.Fprintln(os.Stderr, "  v1/<namespace>/snapshots/pv-snapshotter")
	fmt.Fprintln(os.Stderr, "bucket in every namespace inside one BoltDB transaction.")
	fmt.Fprintln(os.Stderr, "Stop containerd before running with --apply.")
}

// nsPlan describes what we found in one namespace.
type nsPlan struct {
	name        string
	hasPVBucket bool
	entryCount  int
}

type plan struct {
	namespaces []nsPlan
}

// readPlan opens the DB read-only and inspects every namespace's
// snapshots/pv-snapshotter bucket without modifying anything.
func readPlan(dbPath string) (plan, error) {
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		return plan{}, fmt.Errorf("open read-only: %w", err)
	}
	defer db.Close()

	var p plan
	err = db.View(func(tx *bolt.Tx) error {
		v1 := tx.Bucket([]byte(bucketV1))
		if v1 == nil {
			return fmt.Errorf("bucket %q not found", bucketV1)
		}
		return v1.ForEach(func(nsName, val []byte) error {
			if val != nil {
				return nil // not a bucket
			}
			ns := v1.Bucket(nsName)
			if ns == nil {
				return nil
			}
			snapshots := ns.Bucket([]byte(bucketSnapshots))
			entry := nsPlan{name: string(nsName)}
			if snapshots != nil {
				if pv := snapshots.Bucket([]byte(pvSnapshotter)); pv != nil {
					entry.hasPVBucket = true
					_ = pv.ForEach(func(_, _ []byte) error {
						entry.entryCount++
						return nil
					})
				}
			}
			p.namespaces = append(p.namespaces, entry)
			return nil
		})
	})
	if err != nil {
		return plan{}, err
	}
	return p, nil
}

// dropResult describes one bucket that was actually dropped.
type dropResult struct {
	namespace string
	entries   int
}

// dropPVBuckets removes the snapshots/pv-snapshotter bucket from every
// namespace inside a single BoltDB transaction.  bbolt rolls back the
// entire transaction if any individual delete fails.
func dropPVBuckets(dbPath string) ([]dropResult, error) {
	db, err := bolt.Open(dbPath, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open read-write: %w", err)
	}
	defer db.Close()

	var results []dropResult
	err = db.Update(func(tx *bolt.Tx) error {
		v1 := tx.Bucket([]byte(bucketV1))
		if v1 == nil {
			return fmt.Errorf("bucket %q not found", bucketV1)
		}
		// Collect namespace names first to avoid mutating during iteration.
		var nsNames []string
		_ = v1.ForEach(func(nsName, val []byte) error {
			if val == nil {
				nsNames = append(nsNames, string(nsName))
			}
			return nil
		})

		for _, nsName := range nsNames {
			ns := v1.Bucket([]byte(nsName))
			if ns == nil {
				continue
			}
			snapshots := ns.Bucket([]byte(bucketSnapshots))
			if snapshots == nil {
				continue
			}
			pv := snapshots.Bucket([]byte(pvSnapshotter))
			if pv == nil {
				continue
			}
			count := 0
			_ = pv.ForEach(func(_, _ []byte) error {
				count++
				return nil
			})
			if err := snapshots.DeleteBucket([]byte(pvSnapshotter)); err != nil {
				return fmt.Errorf("delete v1/%s/snapshots/pv-snapshotter: %w", nsName, err)
			}
			results = append(results, dropResult{namespace: nsName, entries: count})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// safeBackup copies src to dst and verifies they are byte-identical via SHA-256.
func safeBackup(src, dst string) error {
	srcSum, srcSize, err := sha256File(src)
	if err != nil {
		return fmt.Errorf("hash source: %w", err)
	}
	logf("source sha256: %s (%d bytes)", srcSum, srcSize)

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	dstSum, dstSize, err := sha256File(dst)
	if err != nil {
		return fmt.Errorf("hash backup: %w", err)
	}
	logf("backup sha256: %s (%d bytes)", dstSum, dstSize)
	if srcSum != dstSum || srcSize != dstSize {
		return fmt.Errorf("backup verification failed: source/backup digest mismatch")
	}
	return nil
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func logf(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02T15:04:05Z07:00"), fmt.Sprintf(format, args...))
}

func fatalf(format string, args ...any) {
	logf("FATAL: "+format, args...)
	os.Exit(1)
}
