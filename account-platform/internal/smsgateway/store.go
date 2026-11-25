package smsgateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	uncertain       = "UNCERTAIN"
	accepted        = "ACCEPTED"
	rejected        = "REJECTED"
	recordRetention = 24 * time.Hour
)

type dispatchRecord struct {
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	MessageID   string `json:"message_id,omitempty"`
}

type store struct {
	directory string
	pruneMu   sync.Mutex
	nextPrune time.Time
}

func openStore(directory string) (*store, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("SMS gateway state directory must be absolute")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("SMS gateway requires a private state directory")
	}
	s := &store{directory: directory}
	if err := s.probe(); err != nil {
		return nil, errors.New("SMS gateway state directory is not durable")
	}
	return s, nil
}

func (s *store) probe() error {
	tmp, err := s.stage(dispatchRecord{})
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	link := tmp + ".probe"
	if err := os.Link(tmp, link); err != nil {
		return err
	}
	defer os.Remove(link)
	renamed := link + ".renamed"
	if err := os.Rename(link, renamed); err != nil {
		return err
	}
	defer os.Remove(renamed)
	if err := os.Remove(renamed); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return syncDir(s.directory)
}

func (s *store) filename(id uuid.UUID) string {
	return filepath.Join(s.directory, id.String()+".json")
}

func (s *store) reserve(id uuid.UUID, fingerprint string, now time.Time) (dispatchRecord, bool, error) {
	filename := s.filename(id)
	if record, err := s.read(filename); !errors.Is(err, os.ErrNotExist) {
		return record, false, err
	}
	if err := s.prune(now); err != nil {
		return dispatchRecord{}, false, err
	}
	record := dispatchRecord{Fingerprint: fingerprint, State: uncertain}
	// The claim becomes both visible and durable before contacting Aliyun.
	// A restart after the claim has no authority to dispatch a second time.
	tmp, err := s.stage(record)
	if err != nil {
		return dispatchRecord{}, false, err
	}
	defer os.Remove(tmp)
	if err = os.Link(tmp, filename); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return dispatchRecord{}, false, err
		}
		record, err := s.read(filename)
		return record, false, err
	}
	if err = syncDir(s.directory); err != nil {
		return dispatchRecord{}, false, err
	}
	return record, true, nil
}

func (s *store) read(filename string) (dispatchRecord, error) {
	var record dispatchRecord
	data, err := os.ReadFile(filename)
	if err != nil {
		return record, err
	}
	if len(data) > 1024 || json.Unmarshal(data, &record) != nil || len(record.Fingerprint) != 64 ||
		(record.State != accepted && record.State != uncertain && record.State != rejected) ||
		(record.State == accepted) != receiptID.MatchString(record.MessageID) {
		return dispatchRecord{}, errors.New("invalid SMS dispatch record")
	}
	// A concurrent writer may have renamed the receipt and still be syncing it.
	// Do not publish a terminal result until that rename is durable.
	if record.State != uncertain {
		if err := syncDir(s.directory); err != nil {
			return dispatchRecord{}, err
		}
	}
	return record, nil
}

func (s *store) complete(id uuid.UUID, record dispatchRecord) error {
	tmp, err := s.stage(record)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err = os.Rename(tmp, s.filename(id)); err != nil {
		return err
	}
	return syncDir(s.directory)
}

func (s *store) stage(record dispatchRecord) (string, error) {
	f, err := os.CreateTemp(s.directory, "dispatch-")
	if err != nil {
		return "", err
	}
	if err = json.NewEncoder(f).Encode(record); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(f.Name())
		return "", errors.Join(err, closeErr)
	}
	return f.Name(), nil
}

func (s *store) prune(now time.Time) error {
	s.pruneMu.Lock()
	defer s.pruneMu.Unlock()
	if now.Before(s.nextPrune) {
		return nil
	}
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	var changed bool
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if strings.HasPrefix(e.Name(), "dispatch-") {
			info, err := e.Info()
			if err != nil || !info.ModTime().Before(now.Add(-recordRetention)) {
				continue
			}
		} else {
			id, err := uuid.Parse(strings.TrimSuffix(e.Name(), ".json"))
			if err != nil || id.Version() != 7 || e.Name() != id.String()+".json" {
				continue
			}
			sec, ns := id.Time().UnixTime()
			if !time.Unix(sec, ns).Before(now.Add(-recordRetention)) {
				continue
			}
		}
		if err := os.Remove(filepath.Join(s.directory, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		changed = true
	}
	if changed {
		if err := syncDir(s.directory); err != nil {
			return err
		}
	}
	s.nextPrune = now.Add(time.Hour)
	return nil
}

func syncDir(directory string) error {
	f, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
