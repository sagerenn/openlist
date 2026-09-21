package ttl

import (
	"context"
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
	}
	if deleted > 0 {
		log.Infof("ttl reaper: deleted %d expired file(s)", deleted)
	}
	return deleted
}
