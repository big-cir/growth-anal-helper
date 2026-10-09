package auth

import (
	"fmt"
	"growth-lab/internal/jsstr"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"growth-lab/internal/jsjson"
)

// AuditRecord is one audit event; empty optional fields are left out.
type AuditRecord struct {
	Event  string
	User   *string
	IP     *string
	Target *string
}

// AuditLog writes <outDir>/logs/audit/audit-YYYY-MM-DD.jsonl: allowed fields only, values up to 200 characters.
type AuditLog struct {
	dir string
	now func() time.Time
}

// NewAuditLog returns the audit log of an output folder.
func NewAuditLog(outDir string) *AuditLog {
	return &AuditLog{dir: filepath.Join(outDir, "logs", "audit"), now: time.Now}
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func cut(v *string) any {
	if v == nil {
		return jsjson.Undefined
	}
	return jsstr.U16Slice(*v, 200)
}

// AuditLine is the JSON line written for a record at time t.
func AuditLine(r AuditRecord, t time.Time) string {
	return jsjson.MustStringify(jsjson.Object{{Key: "t", Value: isoTime(t)}, {Key: "event", Value: r.Event}, {Key: "user", Value: cut(r.User)}, {Key: "ip", Value: cut(r.IP)}, {Key: "target", Value: cut(r.Target)}})
}

// Write appends a record. It fails (and the caller refuses the change) if permissions are unsafe.
func (a *AuditLog) Write(r AuditRecord) error {
	if _, err := os.Stat(a.dir); os.IsNotExist(err) {
		if err := os.MkdirAll(a.dir, 0o700); err != nil {
			return fsError("mkdir", a.dir, err)
		}
	}
	me := uid()
	d, err := os.Lstat(a.dir)
	if err != nil {
		return fsError("lstat", a.dir, err)
	}
	if d.Mode()&os.ModeSymlink != 0 || !d.IsDir() || (me >= 0 && ownerOf(d) != me) || d.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s: audit log directory must be owned by me with 0700", a.dir)
	}
	t := a.now()
	f := filepath.Join(a.dir, "audit-"+day(t)+".jsonl")
	line := AuditLine(r, t)
	fd, err := os.OpenFile(f, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fsError("open", f, err)
	}
	defer fd.Close()
	st, err := fd.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || (me >= 0 && ownerOf(st) != me) || st.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s: audit log file must be owned by me with 0600", f)
	}
	_, err = fd.WriteString(line + "\n")
	return err
}

// TryWrite reports write failures to stderr only (e.g. failed sign-ins).
func (a *AuditLog) TryWrite(r AuditRecord) {
	if err := a.Write(r); err != nil {
		slog.Error("audit log write failed", "err", err, "event", r.Event)
	}
}

var auditFileRE = regexp.MustCompile(`^audit-(\d{4}-\d{2}-\d{2})\.jsonl$`)

// Prune deletes files older than the retention.
func (a *AuditLog) Prune(retentionDays int) error {
	if _, err := os.Stat(a.dir); os.IsNotExist(err) {
		return nil
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return fsError("scandir", a.dir, err)
	}
	cutoff := day(time.UnixMilli(a.now().UnixMilli() - int64(retentionDays)*86_400_000))
	for _, e := range entries {
		if m := auditFileRE.FindStringSubmatch(e.Name()); m != nil && m[1] < cutoff {
			p := filepath.Join(a.dir, e.Name())
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fsError("rm", p, err)
			}
		}
	}
	return nil
}
