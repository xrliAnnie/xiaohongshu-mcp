package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func tombstoneFixture(t *testing.T) (string, *writeJournal, writeTombstone) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal")
	journal, err := openWriteJournal(path, "generation-a", true)
	if err != nil {
		t.Fatal(err)
	}
	return path, journal, writeTombstone{ReceiptID: "receipt-a", AttemptID: "attempt-a", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Generation: "generation-a"}
}
func TestWriteTombstoneOneWinnerAcrossIndependentHandles(t *testing.T) {
	path, first, record := tombstoneFixture(t)
	second, err := openWriteJournal(path, "generation-a", false)
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Int32
	var group sync.WaitGroup
	barrier := make(chan struct{})
	for _, journal := range []*writeJournal{first, second} {
		group.Add(1)
		go func(j *writeJournal) {
			defer group.Done()
			<-barrier
			ok, _ := j.consume(record)
			if ok {
				admitted.Add(1)
			}
		}(journal)
	}
	close(barrier)
	group.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admissions=%d", admitted.Load())
	}
	reopened, err := openWriteJournal(path, "generation-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := reopened.consume(record); ok || err != nil {
		t.Fatalf("replay=%v,%v", ok, err)
	}
	record.AttemptID = "other-attempt"
	if ok, err := reopened.consume(record); ok || err == nil {
		t.Fatal("changed attempt was accepted")
	}
}
func TestWriteTombstoneSyncFailureNeverAdmitsOrRetries(t *testing.T) {
	for _, cut := range []string{"file", "directory"} {
		t.Run(cut, func(t *testing.T) {
			path, journal, record := tombstoneFixture(t)
			fail := func(*os.File) error { return errors.New("injected fsync failure") }
			if cut == "file" {
				journal.syncFile = fail
			} else {
				journal.syncDirectory = fail
			}
			if ok, err := journal.consume(record); ok || err == nil {
				t.Fatal("admitted without durable sync")
			}
			reopened, err := openWriteJournal(path, "generation-a", false)
			if err != nil {
				t.Fatal(err)
			}
			if ok, _ := reopened.consume(record); ok {
				t.Fatal("retried a possibly committed receipt")
			}
		})
	}
}
func TestWriteTombstoneMissingGenerationAndUnsafePathsFailClosed(t *testing.T) {
	path, journal, record := tombstoneFixture(t)
	if _, err := openWriteJournal(filepath.Join(t.TempDir(), "missing"), "generation-a", false); err == nil {
		t.Fatal("created missing runtime ledger")
	}
	if _, err := openWriteJournal(path, "other-generation", false); err == nil {
		t.Fatal("accepted stale generation")
	}
	record.ReceiptID = "../escape"
	if ok, err := journal.consume(record); ok || err == nil {
		t.Fatal("accepted traversal")
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	record.ReceiptID = "receipt-a"
	if ok, err := journal.consume(record); ok || err == nil {
		t.Fatal("accepted replaced root")
	}
}
func TestWriteTombstoneCorruptionAndLinksNeverAdmit(t *testing.T) {
	for _, kind := range []string{"corrupt", "symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			path, journal, record := tombstoneFixture(t)
			target := filepath.Join(path, record.ReceiptID+".json")
			switch kind {
			case "corrupt":
				if err := os.WriteFile(target, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), target); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if ok, err := journal.consume(record); !ok || err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, target+"-alias"); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := journal.consume(record); ok || err == nil {
				t.Fatal("accepted unsafe tombstone")
			}
		})
	}
}

func TestWriteTombstoneStartupRejectsCorruptExistingJournal(t *testing.T) {
	path, _, record := tombstoneFixture(t)
	if err := os.WriteFile(filepath.Join(path, record.ReceiptID+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openWriteJournal(path, "generation-a", false); err == nil {
		t.Fatal("opened corrupt existing journal")
	}
}
