package controller

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// BackupDownload streams a consistent SQLite snapshot of the database as a file
// download (admin-only — the backup contains everything, incl. password hashes).
func (c *Controller) BackupDownload(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Dir(c.cfg.DBPath)
	tmp, err := os.CreateTemp(dir, "lira-backup-*.db")
	if err != nil {
		http.Error(w, "backup error", http.StatusInternalServerError)
		return
	}
	path := tmp.Name()
	tmp.Close()
	_ = os.Remove(path) // VACUUM INTO needs a non-existent target
	defer os.Remove(path)

	if err := c.store.Backup(path); err != nil {
		http.Error(w, "backup error", http.StatusInternalServerError)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "backup error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="lira-backup-`+time.Now().Format("2006-01-02")+`.db"`)
	_, _ = io.Copy(w, f)
}
