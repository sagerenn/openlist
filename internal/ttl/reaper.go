package ttl

import (
	"context"
	"strings"
	"time"

	"github.com/sagerenn/openlist/internal/openlist"
	log "github.com/sirupsen/logrus"
)

// Reaper periodically scans for expired TTL records and deletes the
// corresponding files from OpenList.
type Reaper struct {
	Client   *openlist.Client
	Interval time.Duration
	Batch    int
	stop     chan struct{}
	done     chan struct{}
	now      func() time.Time // injectable for tests
}

// NewReaper creates a Reaper. If interval is zero it defaults to 60s; if
// batch is zero it defaults to 100.
func NewReaper(client *openlist.Client, interval time.Duration, batch int) *Reaper {
	if interval == 0 {
		interval = 60 * time.Second
	}
	if batch == 0 {
		batch = 100
	}
	return &Reaper{
		Client:   client,
		Interval: interval,
		Batch:    batch,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		now:      time.Now,
	}
}

// SetNow replaces the clock used to compute "now". Test helper.
func (r *Reaper) SetNow(f func() time.Time) { r.now = f }

// Start launches the reaper goroutine.
func (r *Reaper) Start() {
	go r.loop()
}

// Stop signals the reaper to exit and blocks until it has.
func (r *Reaper) Stop() {
	close(r.stop)
	<-r.done
}

func (r *Reaper) loop() {
	defer close(r.done)
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	r.sweep(context.Background())
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.sweep(context.Background())
		}
	}
}

// SweepOnce performs a single sweep synchronously. Exported for tests and
// manual triggering.
func (r *Reaper) SweepOnce(ctx context.Context) int {
	return r.sweep(ctx)
}

// sweep deletes up to Batch expired files. Returns the number deleted.
func (r *Reaper) sweep(ctx context.Context) int {
	recs, err := Expired(r.now(), r.Batch)
	if err != nil {
		log.Errorf("ttl reaper: query expired: %v", err)
		return 0
	}
	deleted := 0
	// Parents seen deleting from: after the batch completes, the empty ones
	// can be pruned (checked THEN, not per file, so a directory whose
	// remaining expired files are deleted later in this same batch is not
	// left behind merely because it still had entries at that point).
	var parents []string
	for _, rec := range recs {
		if err := r.Client.RemoveFile(ctx, rec.Path); err != nil {
			log.Warnf("ttl reaper: remove %q: %v", rec.Path, err)
			continue
		}
		if err := RemoveRecord(rec.Path); err != nil {
			log.Warnf("ttl reaper: remove record %q: %v", rec.Path, err)
			continue
		}
		deleted++
		if parent := parentDir(rec.Path); parent != "" && parent != "/" {
			parents = append(parents, parent)
		}
	}
	if deleted > 0 {
		log.Infof("ttl reaper: deleted %d expired file(s)", deleted)
	}
	r.pruneEmptyDirs(ctx, parents)
	return deleted
}

// parentDir returns p's parent ("/a/b/c.txt" -> "/a/b"), or "" for a
// root-level name.
func parentDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// pruneEmptyDirs removes the given directories when they are now empty AND
// sit directly under a mount root: pshare-style clients upload into
// per-share directories under a mount ("2018/photo/"), and every file in
// such a directory has the same TTL, so when the last one expires the
// directory is pure clutter. A directory NOT directly under a mount root
// (the mount itself, or a shared structure like "2018/" holding other
// still-live subdirectories) is never pruned, and pruning is best-effort:
// an empty listing is required first because OpenList's remove on a
// directory is a recursive RemoveAll — deleting a non-empty one would take
// live files with it.
func (r *Reaper) pruneEmptyDirs(ctx context.Context, dirs []string) {
	for _, dir := range dirs {
		if mountOf(dir) == "" {
			continue
		}
		n, err := r.Client.ListDirCount(ctx, dir)
		if err != nil || n > 0 {
			continue
		}
		if err := r.Client.RemoveFile(ctx, dir); err != nil {
			log.Debugf("ttl reaper: prune empty dir %q: %v", dir, err)
			continue
		}
		log.Infof("ttl reaper: pruned empty dir %q", dir)
	}
}

// mountOf returns the mount root of p — its first path segment — or "" when
// p has none ("/infini/x" -> "/infini"). It says nothing about whether that
// root is a real storage mount in OpenList; callers use it only as a
// structural guard so the reaper never prunes "/" or root-level dirs.
func mountOf(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.Index(p, "/"); i >= 0 {
		return "/" + p[:i]
	}
	return ""
}
