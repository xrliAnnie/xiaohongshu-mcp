package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"syscall"

	"golang.org/x/sys/unix"
)

var errWriteJournal = errors.New("write_journal_unavailable")
var journalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var journalDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type writeTombstone struct {
	ReceiptID  string `json:"receiptId"`
	AttemptID  string `json:"attemptId"`
	Digest     string `json:"digest"`
	Generation string `json:"generation"`
}

type writeJournal struct {
	path          string
	generation    string
	root          os.FileInfo
	syncFile      func(*os.File) error
	syncDirectory func(*os.File) error
}

// Provisioning is an explicit installation step. Runtime never recreates a lost journal.
func openWriteJournal(path, generation string, provision bool) (*writeJournal, error) {
	if !journalID.MatchString(generation) {
		return nil, errWriteJournal
	}
	if provision {
		if err := os.Mkdir(path, 0700); err != nil {
			return nil, errWriteJournal
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !privateJournalEntry(info, true) {
		return nil, errWriteJournal
	}
	journal := &writeJournal{path: path, generation: generation, root: info, syncFile: (*os.File).Sync, syncDirectory: (*os.File).Sync}
	dir, err := journal.openDirectory()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if provision {
		file, err := createJournalFile(dir, ".generation")
		if err != nil {
			return nil, errWriteJournal
		}
		data := []byte(generation + "\n")
		n, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || n != len(data) || syncErr != nil || closeErr != nil {
			return nil, errWriteJournal
		}
		if err = dir.Sync(); err != nil {
			return nil, errWriteJournal
		}
	}
	marker, err := readJournalFile(dir, ".generation")
	if err != nil || !bytes.Equal(marker, []byte(generation+"\n")) {
		return nil, errWriteJournal
	}
	if err := journal.validateExisting(dir); err != nil {
		return nil, err
	}
	return journal, nil
}
func privateJournalEntry(info os.FileInfo, directory bool) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && stat.Nlink == 1 && info.Size() <= 2048
}
func (j *writeJournal) openDirectory() (*os.File, error) {
	fd, err := unix.Open(j.path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errWriteJournal
	}
	dir := os.NewFile(uintptr(fd), "xhs-journal")
	info, err := dir.Stat()
	if err != nil || !privateJournalEntry(info, true) || !os.SameFile(j.root, info) {
		dir.Close()
		return nil, errWriteJournal
	}
	return dir, nil
}
func createJournalFile(dir *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "xhs-tombstone"), nil
}
func readJournalFile(dir *os.File, name string) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errWriteJournal
	}
	file := os.NewFile(uintptr(fd), "xhs-tombstone")
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !privateJournalEntry(info, false) {
		return nil, errWriteJournal
	}
	data, err := io.ReadAll(io.LimitReader(file, 2049))
	if err != nil || len(data) > 2048 {
		return nil, errWriteJournal
	}
	return data, nil
}

// consume returns true only after O_EXCL creation and file+directory fsync.
// A partial or uncertain write is deliberately retained and can never grant admission.
func (j *writeJournal) consume(record writeTombstone) (bool, error) {
	if !journalID.MatchString(record.ReceiptID) || !journalID.MatchString(record.AttemptID) ||
		record.Generation != j.generation || !journalDigest.MatchString(record.Digest) {
		return false, errWriteJournal
	}
	dir, err := j.openDirectory()
	if err != nil {
		return false, err
	}
	defer dir.Close()
	marker, err := readJournalFile(dir, ".generation")
	if err != nil || !bytes.Equal(marker, []byte(j.generation+"\n")) {
		return false, errWriteJournal
	}
	data, err := json.Marshal(record)
	if err != nil {
		return false, errWriteJournal
	}
	name := record.ReceiptID + ".json"
	file, err := createJournalFile(dir, name)
	if errors.Is(err, unix.EEXIST) {
		stored, readErr := readJournalFile(dir, name)
		if readErr != nil || !bytes.Equal(stored, data) {
			return false, errWriteJournal
		}
		return false, nil
	}
	if err != nil {
		return false, errWriteJournal
	}
	n, writeErr := file.Write(data)
	syncErr := j.syncFile(file)
	closeErr := file.Close()
	if writeErr != nil || n != len(data) || syncErr != nil || closeErr != nil {
		return false, errWriteJournal
	}
	if err = j.syncDirectory(dir); err != nil {
		return false, errWriteJournal
	}
	current, err := os.Lstat(j.path)
	if err != nil || !privateJournalEntry(current, true) || !os.SameFile(j.root, current) {
		return false, errWriteJournal
	}
	return true, nil
}

// A corrupt/foreign entry invalidates startup rather than silently becoming an empty journal.
func (j *writeJournal) validateExisting(dir *os.File) error {
	for {
		entries, err := dir.ReadDir(256)
		if err != nil && err != io.EOF {
			return errWriteJournal
		}
		for _, entry := range entries {
			if entry.Name() == ".generation" {
				continue
			}
			data, readErr := readJournalFile(dir, entry.Name())
			if readErr != nil {
				return errWriteJournal
			}
			var record writeTombstone
			if json.Unmarshal(data, &record) != nil || !journalID.MatchString(record.ReceiptID) ||
				!journalID.MatchString(record.AttemptID) || !journalDigest.MatchString(record.Digest) ||
				record.Generation != j.generation || entry.Name() != record.ReceiptID+".json" {
				return errWriteJournal
			}
			encoded, encodeErr := json.Marshal(record)
			if encodeErr != nil || !bytes.Equal(encoded, data) {
				return errWriteJournal
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}
