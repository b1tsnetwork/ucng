// SPDX-FileCopyrightText: 2026 The UCNG authors (see NOTICE)
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Backups: Der komplette Datenordner wird als .tar.gz gesichert – automatisch einmal täglich
// (um 3 Uhr in der eingestellten Zeitzone) und auf Knopfdruck im Admin-Bereich. Die letzten
// BACKUP_KEEP Sicherungen bleiben erhalten. Wiederherstellen: Server stoppen, Datenordner
// leeren, Archiv darin entpacken (tar -xzf …), Server starten.

type Backups struct {
	src    string // Datenordner
	dir    string // Ziel
	keep   int
	store  Backend                // wird während des Sicherns gesperrt (kein halber Stand)
	flush  func()                 // vorher alles speichern
	export func(dir string) error // Datenbank-Betrieb: Stand im Dateiformat ausgeben

	mu      sync.Mutex
	lastDay string
	lastErr string
}

type BackupInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

var (
	ErrNoBackup  = errors.New("Diese Sicherung gibt es nicht")
	backupNameRe = regexp.MustCompile(`^(ucng|gochat)-\d{8}-\d{6}(-\d+)?\.tar\.gz$`)
)

func NewBackups(src, dir string, keep int) *Backups {
	return &Backups{src: src, dir: dir, keep: keep}
}

// Tick wird regelmäßig aufgerufen und legt einmal täglich ab 3 Uhr eine Sicherung an.
func (b *Backups) Tick(now time.Time, flush func()) {
	day := now.Format("2006-01-02")
	b.mu.Lock()
	if b.lastDay == "" { // nach dem Start: gab es heute schon eine?
		if list, err := b.List(); err == nil && len(list) > 0 && list[0].Created.In(now.Location()).Format("2006-01-02") == day {
			b.lastDay = day
		}
	}
	due := now.Hour() >= 3 && b.lastDay != day
	if due {
		b.lastDay = day
	}
	b.mu.Unlock()
	if !due {
		return
	}
	if flush != nil {
		flush()
	}
	if info, err := b.create(now); err != nil {
		log.Printf("Backup fehlgeschlagen: %v", err)
	} else {
		log.Printf("Backup angelegt: %s (%d KB)", info.Name, info.Size/1024)
	}
}

// Create legt sofort eine Sicherung an.
func (b *Backups) Create(now time.Time) (BackupInfo, error) {
	if b.flush != nil {
		b.flush()
	}
	return b.create(now)
}

func (b *Backups) create(now time.Time) (BackupInfo, error) {
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return BackupInfo{}, err
	}
	base := "ucng-" + now.Format("20060102-150405")
	name := base + ".tar.gz"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(b.dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		name = base + "-" + strconv.Itoa(i) + ".tar.gz"
	}
	tmp, err := os.CreateTemp(b.dir, ".backup-*.tmp")
	if err != nil {
		return BackupInfo{}, err
	}
	defer os.Remove(tmp.Name())
	if b.export != nil {
		err = b.writeExportArchive(tmp)
	} else {
		if b.store != nil {
			b.store.Lock()
		}
		err = b.writeArchive(tmp)
		if b.store != nil {
			b.store.Unlock()
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		b.setErr(err)
		return BackupInfo{}, err
	}
	final := filepath.Join(b.dir, name)
	if err := os.Rename(tmp.Name(), final); err != nil {
		b.setErr(err)
		return BackupInfo{}, err
	}
	b.setErr(nil)
	b.prune()
	st, _ := os.Stat(final)
	info := BackupInfo{Name: name, Created: now.UTC()}
	if st != nil {
		info.Size = st.Size()
	}
	return info, nil
}

func (b *Backups) setErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil {
		b.lastErr = ""
	} else {
		b.lastErr = err.Error()
	}
}

func (b *Backups) LastError() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastErr
}

func (b *Backups) writeArchive(w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := b.addDir(tw, b.src, ""); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// writeExportArchive (Datenbank-Betrieb): Der Stand wird im Dateiformat ausgegeben und
// zusammen mit den hochgeladenen Bildern (media/) gesichert. Ein solches Archiv lässt sich
// mit STORAGE=files direkt starten oder in eine leere Datenbank übernehmen.
func (b *Backups) writeExportArchive(w io.Writer) error {
	tmp, err := os.MkdirTemp(b.dir, ".export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := b.export(tmp); err != nil {
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := b.addDir(tw, tmp, ""); err != nil {
		return err
	}
	if b.src != "" {
		media := filepath.Join(b.src, "media")
		if st, err := os.Stat(media); err == nil && st.IsDir() {
			if err := b.addDir(tw, media, "media"); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// addDir packt einen Ordner ins Archiv (Pfade relativ zu root, mit optionalem Präfix).
// Der Backup-Ordner, temporäre Dateien und SQLite-Dateien werden übersprungen.
func (b *Backups) addDir(tw *tar.Writer, root, prefix string) error {
	absDir, _ := filepath.Abs(b.dir)
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs, _ := filepath.Abs(path); abs == absDir {
			return filepath.SkipDir // Backup-Ordner liegt im Datenordner
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if prefix != "" {
			rel = filepath.Join(prefix, rel)
		} else if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") && strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		if n := d.Name(); strings.HasSuffix(n, ".db") || strings.HasSuffix(n, ".db-wal") || strings.HasSuffix(n, ".db-shm") || strings.HasSuffix(n, ".db-journal") {
			return nil // eine laufende SQLite-Datenbank lässt sich nicht als Datei sichern
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
}

// List liefert alle Sicherungen, neueste zuerst.
func (b *Backups) List() ([]BackupInfo, error) {
	entries, err := os.ReadDir(b.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []BackupInfo{}
	for _, e := range entries {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Name: e.Name(), Size: info.Size(), Created: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].Name > out[j].Name
	})
	return out, nil
}

func (b *Backups) prune() {
	list, err := b.List()
	if err != nil || b.keep <= 0 || len(list) <= b.keep {
		return
	}
	for _, old := range list[b.keep:] {
		os.Remove(filepath.Join(b.dir, old.Name))
	}
}

func (b *Backups) Path(name string) (string, error) {
	if !backupNameRe.MatchString(name) {
		return "", ErrNoBackup
	}
	p := filepath.Join(b.dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", ErrNoBackup
	}
	return p, nil
}

// --- Admin-API ---

var errNoBackups = errors.New("Backups sind aus (kein Datenordner oder BACKUP_KEEP=0)")

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "backups": []BackupInfo{}})
		return
	}
	list, err := s.backups.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "backups": list, "keep": s.backups.keep,
		"dir": s.backups.dir, "lastError": s.backups.LastError()})
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		writeErr(w, http.StatusConflict, errNoBackups.Error())
		return
	}
	info, err := s.backups.Create(time.Now().In(s.hub.Location()))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Sicherung fehlgeschlagen: "+err.Error())
		return
	}
	s.log(r, AuditEntry{Action: "backup.create", Detail: info.Name})
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil {
		writeErr(w, http.StatusNotFound, errNoBackups.Error())
		return
	}
	p, err := s.backups.Path(r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	s.log(r, AuditEntry{Action: "backup.download", Detail: filepath.Base(p)})
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(p)+`"`)
	http.ServeFile(w, r, p)
}
